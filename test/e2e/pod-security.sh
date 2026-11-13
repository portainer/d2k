#!/usr/bin/env bash
# Pod Security checks: under the namespace's Pod Security level, a container
# whose Docker settings comply runs, and one whose settings break the level is
# refused with Kubernetes' reason. d2k never changes the level: this script
# sets it, as a cluster admin or Portainer's Pod Security Standards policy
# would, and restores the namespace's own level on exit. Defaults match run.sh (the kind cluster
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

# The namespace's own levels, put back on exit: a namespace may already
# enforce one (Talos, Portainer's policy) when NAMESPACE names it.
ENFORCE_LABEL=pod-security.kubernetes.io/enforce
ORIGINAL_ENFORCE=$(k get namespace "$NAMESPACE" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}')
WARN_LABEL=pod-security.kubernetes.io/warn
ORIGINAL_WARN=$(k get namespace "$NAMESPACE" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/warn}')

# restore_label KEY VALUE: set the namespace's label back to VALUE, or
# remove it when VALUE is empty.
restore_label() {
  if [ -n "$2" ]; then
    k label namespace "$NAMESPACE" "$1=$2" --overwrite >/dev/null
  else
    k label namespace "$NAMESPACE" "$1-" >/dev/null
  fi
}

cleanup() {
  docker rm -f e2e-psa-form e2e-psa-plain e2e-psa-priv e2e-psa-sysadmin e2e-psa-netraw e2e-psa-always e2e-psa-create e2e-psa-old e2e-psa-nonroot e2e-psa-root e2e-psa-warn >/dev/null 2>&1
  docker compose -p e2e-psa -f "$WORKDIR/compose.yaml" down >/dev/null 2>&1
  docker compose -p e2e-psa-warn -f "$WORKDIR/compose-warn.yaml" down >/dev/null 2>&1
  restore_label "$ENFORCE_LABEL" "$ORIGINAL_ENFORCE"
  restore_label "$WARN_LABEL" "$ORIGINAL_WARN"
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

enforce() { k label namespace "$NAMESPACE" "$ENFORCE_LABEL=$1" --overwrite >/dev/null; }

# info_shows LEVEL [WARNING]: docker info reports the level, and the
# warning when given. d2k keeps the level for a minute, so wait for it.
info_shows() {
  local i
  for i in $(seq 1 15); do
    docker info >"$WORKDIR/info" 2>&1
    if grep -q "Profile: $1" "$WORKDIR/info" && { [ -z "${2:-}" ] || grep -q "WARNING: .*$2" "$WORKDIR/info"; }; then
      return 0
    fi
    sleep 5
  done
  grep -A1 -i "pod-security\|WARNING" "$WORKDIR/info"
  return 1
}

# deployment_absent NAME: d2k created no Deployment for the container.
deployment_absent() { ! k -n "$NAMESPACE" get deployment "$1" >/dev/null 2>&1; }

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

cat >"$WORKDIR/compose-warn.yaml" <<'EOF'
services:
  app:
    image: busybox
    command: sleep 300
EOF

echo "== pod security ($STANDALONE, $CONTEXT, namespace $NAMESPACE)"

check "info: docker info shows the privileged level, with no warning" bash -c '
  docker info 2>&1 | grep -q "Profile: privileged" && ! docker info 2>&1 | grep -q "WARNING: namespace"'

# Created before the namespace's level was raised: refused at start.
docker create --name e2e-psa-old --privileged busybox sleep 300 >/dev/null

enforce baseline

check "info: docker info shows baseline, and what it refuses" info_shows baseline "enforces Pod Security \"baseline\""

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

# A refused container is refused at create, before anything exists, with
# the Docker flag to change.
check "baseline: a refused create names the flag" bash -c '
  ! docker create --name e2e-psa-create --privileged busybox sleep 300 >'"$WORKDIR"'/create 2>&1 &&
  grep -q "remove --privileged" '"$WORKDIR"'/create || { cat '"$WORKDIR"'/create; exit 1; }'
check "baseline: a refused create creates nothing" deployment_absent e2e-psa-create
check "baseline: a container created before the level is refused at start, and stays stopped" bash -c '
  ! docker start e2e-psa-old >'"$WORKDIR"'/start 2>&1 &&
  grep -q "remove --privileged" '"$WORKDIR"'/start &&
  [ "$(docker inspect -f "{{.State.Running}}" e2e-psa-old)" = false ] || { cat '"$WORKDIR"'/start; exit 1; }'

docker rm -f e2e-psa-form e2e-psa-plain e2e-psa-priv e2e-psa-sysadmin e2e-psa-netraw e2e-psa-always e2e-psa-create e2e-psa-old >/dev/null 2>&1

enforce restricted

check "info: docker info shows restricted, and what to set" info_shows restricted "cap-drop ALL"

check "restricted: a non-root container with every capability dropped runs" bash -c '
  docker run -d --name e2e-psa-nonroot --user 1000:1000 --cap-drop ALL --security-opt no-new-privileges busybox sleep 300 >/dev/null &&
  [ "$(docker inspect -f "{{.State.Running}}" e2e-psa-nonroot)" = true ]'

check "restricted: the compose equivalent runs" bash -c '
  docker compose -p e2e-psa -f '"$WORKDIR"'/compose.yaml up -d --wait >/dev/null 2>&1 &&
  [ "$(docker compose -p e2e-psa -f '"$WORKDIR"'/compose.yaml ps -q app | xargs docker inspect -f "{{.State.Running}}")" = true ]'

check "restricted: a plain docker run is refused with Kubernetes' reason" refused e2e-psa-root 'violates PodSecurity "restricted' busybox sleep 300
check "restricted: the refusal names the flags" bash -c '
  grep -q "add --cap-drop ALL" '"$WORKDIR"'/run && grep -q "user 1000:1000" '"$WORKDIR"'/run &&
  grep -q "no-new-privileges" '"$WORKDIR"'/run || { cat '"$WORKDIR"'/run; exit 1; }'

# Talos' default: baseline enforced, restricted warned about. A plain
# container runs, and the client shows what restricted would refuse.
enforce baseline
k label namespace "$NAMESPACE" "$WARN_LABEL=restricted" --overwrite >/dev/null

check "warn: docker run shows Pod Security's warning, and runs" bash -c '
  docker run -d --name e2e-psa-warn busybox sleep 300 >/dev/null 2>'"$WORKDIR"'/warn &&
  grep -q "would refuse this container" '"$WORKDIR"'/warn &&
  [ "$(docker inspect -f "{{.State.Running}}" e2e-psa-warn)" = true ] || { cat '"$WORKDIR"'/warn; exit 1; }'

check "warn: compose shows Pod Security's warning, and runs" bash -c '
  docker compose -p e2e-psa-warn -f '"$WORKDIR"'/compose-warn.yaml up -d --wait >'"$WORKDIR"'/warn 2>&1 &&
  grep -q "would refuse this container" '"$WORKDIR"'/warn || { cat '"$WORKDIR"'/warn; exit 1; }'

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
