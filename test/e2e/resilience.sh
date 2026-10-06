#!/usr/bin/env bash
# Resilience checks: d2k restarting or losing a pod mid-operation must not
# leave a client hanging, an operation half-done, or a container's state
# misreported. Defaults target the resilience lane's own cluster (live2);
# override CONTEXT/STANDALONE/PKI to run against another environment, as
# run.sh does.
set -uo pipefail

CONTEXT=kind-${CLUSTER:-d2k-live2}
STANDALONE=${STANDALONE:-tcp://localhost:31312}
HTTPS_STANDALONE=https://${STANDALONE#tcp://}
WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT

PKI=${PKI:-/Users/steven/go/d2k/.overnight/2026-10-07/env/pki-live2/client}
export DOCKER_HOST=$STANDALONE DOCKER_TLS_VERIFY=1 DOCKER_CERT_PATH=$PKI CURL_HOME=$WORKDIR
printf 'cacert = "%s/ca.pem"\ncert = "%s/cert.pem"\nkey = "%s/key.pem"\n' "$PKI" "$PKI" "$PKI" >"$WORKDIR/.curlrc"

pass=0 fail=0
failed=()

k() { kubectl --context "$CONTEXT" "$@"; }

check() {
  local name=$1; shift
  if ("$@") >"$WORKDIR/out" 2>&1; then
    echo "PASS   $name"; pass=$((pass + 1))
  else
    echo "FAIL   $name"; sed 's/^/         /' "$WORKDIR/out" | tail -5
    fail=$((fail + 1)); failed+=("$name")
  fi
}

echo "== resilience ($STANDALONE, $CONTEXT)"

# D2K-022: the preStop hook must actually run (the image has no shell, so a
# bare "sleep" exec silently never delays SIGTERM at all) and SIGTERM must
# not reach d2k before kube-proxy has had time to stop routing to it.
check "restart leaves no FailedPreStopHook event" bash -c '
  k() { kubectl --context '"$CONTEXT"' "$@"; }
  since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  k -n d2k rollout restart deployment/d2k >/dev/null &&
  k -n d2k rollout status deployment/d2k --timeout=60s >/dev/null &&
  sleep 2 &&
  [ -z "$(k -n d2k get events -o json | python3 -c "
import json,sys
d=json.load(sys.stdin)
for i in d[\"items\"]:
    if i.get(\"reason\")==\"FailedPreStopHook\" and i.get(\"lastTimestamp\",\"\") >= \"'"\$since"'\":
        print(i[\"reason\"])
")" ]'

check "docker version has no dropped call across a pod delete" bash -c '
  k() { kubectl --context '"$CONTEXT"' "$@"; }
  (for _ in $(seq 1 40); do docker version -f "{{.Server.Version}}" || echo FAILED; done) >'"$WORKDIR"'/loop.out &
  loop=$!
  sleep 1
  k -n d2k delete pod -l app=d2k --wait=false >/dev/null
  wait $loop
  ! grep -q FAILED '"$WORKDIR"'/loop.out'

# D2K-026: a run-once container whose pod disappears some way other than
# d2k'"'"'s own stop (a force delete, standing in for a node loss) must not be
# reported as a clean Exited(0): that hides that it never finished.
check "a force-deleted run-once container isn't Exited(0)" bash -c '
  k() { kubectl --context '"$CONTEXT"' "$@"; }
  docker run -d --name e2e-resilience-once busybox sh -c "sleep 30; exit 7" >/dev/null &&
  pod=$(k -n d2k get pods -l d2k.portainer.io/name=e2e-resilience-once -o jsonpath="{.items[0].metadata.name}") &&
  k -n d2k delete pod "$pod" --grace-period=0 --force >/dev/null 2>&1 &&
  for _ in $(seq 1 15); do
    [ "$(docker inspect -f {{.State.Status}} e2e-resilience-once 2>/dev/null)" = exited ] && break
    sleep 1
  done &&
  code=$(docker inspect -f {{.State.ExitCode}} e2e-resilience-once) &&
  docker rm -f e2e-resilience-once >/dev/null 2>&1
  [ "$code" != 0 ]'

# Restarting d2k mid compose-up must still converge, and a clean compose
# down afterwards must leave nothing behind.
check "compose up/down converges across a restart, no orphans" bash -c '
  k() { kubectl --context '"$CONTEXT"' "$@"; }
  proj='"$WORKDIR"'/proj
  mkdir -p "$proj"
  cat >"$proj/compose.yaml" <<EOF
services:
  web:
    image: nginx:alpine
  redis:
    image: redis:7-alpine
EOF
  ( cd "$proj" && docker compose -p e2e-resilience up -d --quiet-pull ) >/dev/null &
  up=$!
  sleep 2
  k -n d2k rollout restart deployment/d2k >/dev/null
  wait $up || exit 1
  for _ in $(seq 1 30); do
    [ "$(docker inspect -f {{.State.Status}} e2e-resilience-web 2>/dev/null)" = running ] &&
    [ "$(docker inspect -f {{.State.Status}} e2e-resilience-redis 2>/dev/null)" = running ] && break
    sleep 2
  done &&
  ( cd "$proj" && docker compose -p e2e-resilience down ) >/dev/null &&
  [ -z "$(k -n d2k get deploy,svc,pvc -l d2k.portainer.io/compose-project=e2e-resilience -o name)" ] &&
  [ -z "$(docker ps -aq --filter name=e2e-resilience)" ]'

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
