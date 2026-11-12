#!/usr/bin/env bash
# Pod Security checks: under the namespace's Pod Security level, a container
# whose Docker settings comply runs, and one whose settings break the level is
# refused with Kubernetes' reason. d2k never changes the level: this script
# sets it, as a cluster admin or Portainer's Pod Security Standards policy
# would, and removes it on exit. Defaults match run.sh (the kind cluster
# setup.sh creates); override CLUSTER/STANDALONE/PKI/NAMESPACE to run against
# another environment.
set -uo pipefail

CONTEXT=kind-${CLUSTER:-d2k}
NAMESPACE=${NAMESPACE:-d2k}
STANDALONE=${STANDALONE:-tcp://localhost:31112}
HTTPS_STANDALONE=https://${STANDALONE#tcp://}
WORKDIR=$(mktemp -d)

PKI=${PKI:-$(cd "$(dirname "$0")" && pwd)/.pki/client}
export DOCKER_HOST=$STANDALONE DOCKER_TLS_VERIFY=1 DOCKER_CERT_PATH=$PKI CURL_HOME=$WORKDIR
printf 'cacert = "%s/ca.pem"\ncert = "%s/cert.pem"\nkey = "%s/key.pem"\n' "$PKI" "$PKI" "$PKI" >"$WORKDIR/.curlrc"

pass=0 fail=0

k() { kubectl --context "$CONTEXT" "$@"; }

cleanup() {
  docker rm -f e2e-psa-form e2e-psa-plain e2e-psa-priv e2e-psa-sysadmin e2e-psa-netraw e2e-psa-always e2e-psa-nonroot e2e-psa-root >/dev/null 2>&1
  docker compose -p e2e-psa -f "$WORKDIR/compose.yaml" down >/dev/null 2>&1
  k label namespace "$NAMESPACE" pod-security.kubernetes.io/enforce- >/dev/null
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

check() {
  local name=$1; shift
  if ("$@") >"$WORKDIR/out" 2>&1; then
    echo "PASS   $name"; pass=$((pass + 1))
  else
    echo "FAIL   $name"; sed 's/^/         /' "$WORKDIR/out" | tail -5
    fail=$((fail + 1))
  fi
}

enforce() { k label namespace "$NAMESPACE" "pod-security.kubernetes.io/enforce=$1" --overwrite >/dev/null; }

running() { [ "$(docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null)" = true ]; }

# refused NAME REASON ARGS...: docker run fails with Kubernetes' reason.
refused() {
  local name=$1 reason=$2; shift 2
  ! docker run -d --name "$name" "$@" >"$WORKDIR/run" 2>&1 && grep -q "$reason" "$WORKDIR/run" || { cat "$WORKDIR/run"; return 1; }
}

# The create request Portainer's form sends with its default capabilities:
# Docker's defaults added, its other capabilities dropped.
cat >"$WORKDIR/form.json" <<'EOF'
{"Image":"nginx:alpine","HostConfig":{"RestartPolicy":{"Name":"no"},
 "CapAdd":["SETPCAP","MKNOD","AUDIT_WRITE","CHOWN","NET_RAW","DAC_OVERRIDE","FOWNER","FSETID","KILL","SETGID","SETUID","NET_BIND_SERVICE","SYS_CHROOT","SETFCAP"],
 "CapDrop":["AUDIT_CONTROL","BLOCK_SUSPEND","DAC_READ_SEARCH","IPC_LOCK","IPC_OWNER","LEASE","LINUX_IMMUTABLE","MAC_ADMIN","MAC_OVERRIDE","NET_ADMIN","NET_BROADCAST","SYSLOG","SYS_ADMIN","SYS_BOOT","SYS_MODULE","SYS_NICE","SYS_PACCT","SYS_PTRACE","SYS_RAWIO","SYS_RESOURCE","SYS_TIME","SYS_TTY_CONFIG","WAKE_ALARM"]}}
EOF

cat >"$WORKDIR/compose.yaml" <<'EOF'
services:
  app:
    image: busybox
    command: sleep 300
    user: "1000:1000"
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true]
EOF

echo "== pod security ($STANDALONE, $CONTEXT, namespace $NAMESPACE)"

enforce baseline

check "baseline: a container from Portainer's form runs" bash -c '
  curl -sf -X POST -H "Content-Type: application/json" --data @'"$WORKDIR"'/form.json \
    "'"$HTTPS_STANDALONE"'/v1.52/containers/create?name=e2e-psa-form" >/dev/null &&
  curl -sf -X POST "'"$HTTPS_STANDALONE"'/v1.52/containers/e2e-psa-form/start" &&
  [ "$(docker inspect -f "{{.State.Running}}" e2e-psa-form)" = true ]'

check "baseline: a plain docker run runs" bash -c '
  docker run -d --name e2e-psa-plain nginx:alpine >/dev/null &&
  [ "$(docker inspect -f "{{.State.Running}}" e2e-psa-plain)" = true ]'

check "baseline: --privileged is refused with Kubernetes' reason" refused e2e-psa-priv 'violates PodSecurity "baseline' --privileged busybox sleep 300
check "baseline: --cap-add SYS_ADMIN is refused with Kubernetes' reason" refused e2e-psa-sysadmin 'SYS_ADMIN' --cap-add SYS_ADMIN busybox sleep 300
# NET_RAW is one of Docker's default capabilities: adding it changes nothing
# in Docker, so d2k leaves it out of the pod, and the runtime's default
# grants it as baseline allows.
check "baseline: --cap-add NET_RAW runs, as adding a default does nothing" bash -c '
  docker run -d --name e2e-psa-netraw --cap-add NET_RAW nginx:alpine >/dev/null &&
  [ "$(docker inspect -f "{{.State.Running}}" e2e-psa-netraw)" = true ]'
check "baseline: --privileged --restart always is refused" refused e2e-psa-always 'PodSecurity' --restart always --privileged busybox sleep 300

docker rm -f e2e-psa-form e2e-psa-plain e2e-psa-priv e2e-psa-sysadmin e2e-psa-netraw e2e-psa-always >/dev/null 2>&1

enforce restricted

check "restricted: a non-root container with every capability dropped runs" bash -c '
  docker run -d --name e2e-psa-nonroot --user 1000:1000 --cap-drop ALL --security-opt no-new-privileges busybox sleep 300 >/dev/null &&
  [ "$(docker inspect -f "{{.State.Running}}" e2e-psa-nonroot)" = true ]'

check "restricted: the compose equivalent runs" bash -c '
  docker compose -p e2e-psa -f '"$WORKDIR"'/compose.yaml up -d --wait >/dev/null 2>&1 &&
  [ "$(docker compose -p e2e-psa -f '"$WORKDIR"'/compose.yaml ps -q app | xargs docker inspect -f "{{.State.Running}}")" = true ]'

check "restricted: a plain docker run is refused with Kubernetes' reason" refused e2e-psa-root 'violates PodSecurity "restricted' busybox sleep 300

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
