# Helpers for upgrade-1.2.sh, upgrade.sh and install-lifecycle.sh: each
# installs d2k into its own namespaces of a kind cluster, checks it as a
# Docker client would, and removes what it created on exit.
# shellcheck shell=bash

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
CLUSTER=${CLUSTER:-d2k}
CONTEXT=kind-$CLUSTER
WORKDIR=$(mktemp -d)
pass=0 fail=0
namespaces=() forwards=() connected=()

check() {
	local name=$1
	shift
	reconnect
	if "$@" >/dev/null 2>&1; then
		echo "PASS   $name"
		pass=$((pass + 1))
	else
		echo "FAIL   $name"
		fail=$((fail + 1))
	fi
}

k() { kubectl --context "$CONTEXT" "$@"; }

# image <git ref or "working-tree"> <tag>: build d2k at ref and load it
# into the kind cluster.
image() {
	local src=$ROOT
	if [ "$1" != working-tree ]; then
		src=$WORKDIR/src-$2
		git -C "$ROOT" worktree add --detach "$src" "$1" >/dev/null 2>&1 || return 1
	fi
	env -u DOCKER_HOST -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH \
		docker build -q -t "d2k:$2" --build-arg VERSION="$2" "$src" >/dev/null || return 1
	kind load docker-image "d2k:$2" --name "$CLUSTER" >/dev/null
}

# render <manifest> <namespace> <image tag> <swarm mode> [tls sans]: the
# manifest for one install, as README says to change it.
render() {
	sed -e "s/^\(  name: \)d2k\(   # change to your target namespace\)/\1$2\2/" \
		-e "s/namespace: d2k   #/namespace: $2   #/" \
		-e "s/d2k-node-reader/d2k-node-reader-$2/" \
		-e "s#image: portainer/d2k:latest.*#image: d2k:$3#" \
		-e "s/imagePullPolicy: Always/imagePullPolicy: IfNotPresent/" \
		-e "s/value: \"true\"   # set to \"false\"/value: \"$4\"   # set to \"false\"/" \
		-e "s/value: \"false\"   # set to \"true\"/value: \"$4\"   # set to \"true\"/" \
		-e "s/value: \"\"   # e.g./value: \"${5:-localhost}\"   # e.g./" \
		"$1"
}

# install <manifest> <namespace> <image tag> <swarm mode>
install() {
	# A previous run's namespace may still be terminating.
	for _ in $(seq 60); do
		[ "$(k get namespace "$2" -o jsonpath='{.status.phase}' 2>/dev/null)" = Terminating ] || break
		sleep 2
	done
	namespaces+=("$2")
	render "$@" | k apply -f - >/dev/null && k -n "$2" rollout status deploy/d2k --timeout=180s >/dev/null
}

# connect <namespace> <service port> <local port> [cert dir]: point the
# Docker CLI at the install through a port-forward, with TLS when cert dir
# is given, and wait until it answers.
connect() {
	connected=("$@")
	pkill -f "port-forward svc/d2k $3:" 2>/dev/null
	kubectl --context "$CONTEXT" -n "$1" port-forward svc/d2k "$3:$2" >/dev/null 2>&1 &
	forwards+=($!)
	export DOCKER_HOST=tcp://localhost:$3
	if [ -n "${4:-}" ]; then
		export DOCKER_TLS_VERIFY=1 DOCKER_CERT_PATH=$4
	else
		unset DOCKER_TLS_VERIFY DOCKER_CERT_PATH
	fi
	for _ in $(seq 30); do
		docker version >/dev/null 2>&1 && return 0
		sleep 1
	done
	return 1
}

# reconnect: kubectl port-forward drops now and then; set up the last
# connection again if d2k doesn't answer through it.
reconnect() {
	[ ${#connected[@]} -gt 0 ] && ! docker version >/dev/null 2>&1 && connect "${connected[@]}"
	return 0
}

# dk: docker, reconnecting first if needed (for setup steps).
dk() {
	reconnect
	docker "$@"
}

# client_cert <namespace>: the client certificate d2k created, as README
# says to fetch it; prints the directory.
client_cert() {
	mkdir -p "$WORKDIR/client-$1"
	for f in ca cert key; do
		k -n "$1" get secret d2k-client -o jsonpath="{.data.$f\.pem}" | base64 -d >"$WORKDIR/client-$1/$f.pem"
	done
	echo "$WORKDIR/client-$1"
}

cleanup() {
	# "${a[@]+...}": bash 3.2 (macOS) treats an empty array as unset under set -u.
	for pid in ${forwards[@]+"${forwards[@]}"}; do kill "$pid" 2>/dev/null; done
	for ns in ${namespaces[@]+"${namespaces[@]}"}; do
		k delete namespace "$ns" --wait=false >/dev/null 2>&1
		k delete clusterrolebinding,clusterrole "d2k-node-reader-$ns" >/dev/null 2>&1
	done
	for wt in "$WORKDIR"/src-*; do
		[ -d "$wt" ] && git -C "$ROOT" worktree remove --force "$wt"
	done
	rm -rf "$WORKDIR"
}
trap cleanup EXIT

result() {
	echo
	echo "pass=$pass fail=$fail"
	[ "$fail" -eq 0 ]
}
