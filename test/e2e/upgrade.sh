#!/usr/bin/env bash
# Upgrade an install of an earlier mTLS build of d2k in place to the
# working tree's build, in Docker host mode with the admin's own
# certificates, and check that containers, volumes, networks and compose
# projects come through unchanged.
#
#   FROM=<git ref> test/e2e/upgrade.sh      # e.g. FROM=4817fd6
#
# CLUSTER picks another kind cluster. For 1.2.x, use upgrade-1.2.sh.
set -uo pipefail
. "$(dirname "$0")/upgrade-lib.sh"

FROM=${FROM:?set FROM to the git ref to upgrade from}
NS=e2e-up

echo "== build $FROM and the working tree"
image "$FROM" earlier && image working-tree upgrade || { echo "build failed"; exit 1; }

echo "== install $FROM ($NS, host mode, own certificates)"
(cd "$ROOT" && go run ./cmd pki init --san localhost,127.0.0.1 --out "$WORKDIR/pki" >/dev/null) || exit 1
for _ in $(seq 60); do
	[ "$(k get namespace "$NS" -o jsonpath='{.status.phase}' 2>/dev/null)" = Terminating ] || break
	sleep 2
done
k create namespace "$NS" >/dev/null
k -n "$NS" create secret generic d2k-tls --from-file="$WORKDIR/pki/server" >/dev/null
install "$WORKDIR/src-earlier/deploy/kubernetes.yaml" "$NS" earlier false || { echo "install failed"; exit 1; }
connect "$NS" 2376 26770 "$WORKDIR/pki/client" || { echo "$FROM unreachable"; exit 1; }
cat >"$WORKDIR/compose.yaml" <<YAML
services:
  app:
    image: nginx:alpine
    ports: ["18981:80"]
  cache:
    image: redis:7-alpine
    volumes: [cachedata:/data]
volumes:
  cachedata:
YAML
dk network create frontnet >/dev/null
dk run -d --name web --network frontnet -p 18980:80 nginx:alpine >/dev/null
dk run -d --name always --restart always nginx:alpine >/dev/null
dk run --name once busybox:1.36 sh -c 'echo ran-once' >/dev/null
dk compose -p proj -f "$WORKDIR/compose.yaml" up -d --wait >/dev/null 2>&1
dk exec proj-cache-1 redis-cli set k kept >/dev/null
dk stop always >/dev/null
ids_before=$(dk ps -aq --no-trunc | sort)
echo "$FROM state:"
dk ps -a --format '  {{.Names}} {{.Status}}'
[ "$(echo "$ids_before" | wc -w)" -eq 5 ] || { echo "starting state incomplete"; exit 1; }

echo "== upgrade"
k apply -f "$ROOT/deploy/admission-policy.yaml" >/dev/null
install "$ROOT/deploy/kubernetes.yaml" "$NS" upgrade false || { echo "upgrade failed"; exit 1; }
check "the admin's certificates still work" connect "$NS" 2376 26771 "$WORKDIR/pki/client"
check "d2k kept the admin's d2k-tls, created no d2k-client" bash -c "! kubectl --context $CONTEXT -n $NS get secret d2k-client"
check "same containers (IDs unchanged)" bash -c "[ \"\$(docker ps -aq --no-trunc | sort)\" = \"$ids_before\" ]"
check "nothing migrated (already recorded)" bash -c "! kubectl --context $CONTEXT -n $NS logs deploy/d2k | grep -q 'Recorded the Docker name'"
check "running container running, port published" bash -c '[ "$(docker inspect -f {{.State.Status}} web)" = running ] && docker port web | grep -q 18980'
check "network kept, container still on it" bash -c "docker network inspect frontnet -f '{{json .Containers}}' | grep -q web"
check "stopped restart-always container exited, starts" bash -c '[ "$(docker inspect -f {{.State.Status}} always)" = exited ] && docker start always >/dev/null'
check "run-once container exited 0, start -a reruns it" bash -c '[ "$(docker inspect -f {{.State.ExitCode}} once)" = 0 ] && docker start -a once | grep -q ran-once'
check "volume data kept" bash -c '[ "$(docker exec proj-cache-1 redis-cli get k)" = kept ]'
check "compose ls and ps" bash -c "docker compose ls | grep -q '^proj ' && [ \$(docker compose -p proj ps -q | wc -l) -eq 2 ]"
check "compose up from the same file changes nothing" bash -c "before=\$(docker compose -p proj ps -q | sort); docker compose -p proj -f $WORKDIR/compose.yaml up -d --wait >/dev/null 2>&1; [ \"\$(docker compose -p proj ps -q | sort)\" = \"\$before\" ]"
check "logs of a stopped container" bash -c "docker stop web >/dev/null && docker logs web 2>&1 | grep -qi nginx"
check "compose down -v, rm, network rm" bash -c "docker compose -p proj -f $WORKDIR/compose.yaml down -v >/dev/null 2>&1 && docker rm -f web always once >/dev/null && docker network rm frontnet >/dev/null"
result
