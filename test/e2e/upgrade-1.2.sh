#!/usr/bin/env bash
# Upgrade a d2k 1.2.x install in place to the working tree's build, as
# README's "Upgrading from d2k 1.2.x" says, and check it as a Docker client
# would. 1.2.x served plain TCP: the upgrade applies the new manifest with
# no d2k-tls Secret, so d2k creates its certificates.
#
#   test/e2e/upgrade-1.2.sh                 # 1.2.3, Docker host mode
#   SWARM=true test/e2e/upgrade-1.2.sh      # 1.2.3 in Swarm mode, kept
#   SWARM=true AFTER=false test/e2e/upgrade-1.2.sh
#                                           # Swarm install upgraded to host mode
#
# FROM picks another 1.2.x tag, CLUSTER another kind cluster.
set -uo pipefail
. "$(dirname "$0")/upgrade-lib.sh"

FROM=${FROM:-1.2.3} SWARM=${SWARM:-false} AFTER=${AFTER:-$SWARM}
NS=e2e-up12-$SWARM-$AFTER

echo "== build $FROM and the working tree"
image "$FROM" legacy && image working-tree upgrade || { echo "build failed"; exit 1; }

echo "== install $FROM ($NS, swarm mode $SWARM)"
install "$WORKDIR/src-legacy/deploy/kubernetes.yaml" "$NS" legacy "$SWARM" || { echo "install failed"; exit 1; }
connect "$NS" 2375 26750 || { echo "$FROM unreachable"; exit 1; }
cat >"$WORKDIR/compose.yaml" <<YAML
services:
  app:
    image: nginx:alpine
  cache:
    image: redis:7-alpine
    volumes: [cachedata:/data]
volumes:
  cachedata:
YAML
if [ "$SWARM" = false ]; then
	dk volume create data_vol >/dev/null
	dk run -d --name keep nginx:alpine >/dev/null
	dk run -d --name stopme nginx:alpine >/dev/null
	dk compose -p proj -f "$WORKDIR/compose.yaml" up -d >/dev/null 2>&1
	sleep 20
	dk stop stopme >/dev/null
else
	dk service create -d --name websvc --replicas 2 nginx:alpine >/dev/null
	printf v1 | dk secret create mysecret - >/dev/null
	printf cfg | dk config create myconfig - >/dev/null
	dk stack deploy -c "$WORKDIR/compose.yaml" stk >/dev/null 2>&1
	sleep 20
	dk service ls --format '{{.Name}}' | sort >"$WORKDIR/services"
fi
echo "$FROM state:"
dk ps -a --format '  {{.Names}} {{.Status}}'

echo "== upgrade (swarm mode $AFTER)"
k apply -f "$ROOT/deploy/admission-policy.yaml" >/dev/null
install "$ROOT/deploy/kubernetes.yaml" "$NS" upgrade "$AFTER" || { echo "upgrade failed"; exit 1; }
check "d2k created its certificates (no d2k-tls Secret before)" k -n "$NS" get secret d2k-tls d2k-client
check "mTLS with the fetched client certificate" connect "$NS" 2376 26760 "$(client_cert "$NS")"
check "plain TCP is gone" bash -c "! curl -s -m 5 http://localhost:26760/_ping | grep -q OK"
if [ "$SWARM" = true ] && [ "$AFTER" = false ]; then
	check "host mode warns about the Swarm services it doesn't list" bash -c "kubectl --context $CONTEXT -n $NS logs deploy/d2k | grep -q 'set D2K_SWARM_MODE=true'"
	check "Swarm objects left as they are" bash -c "[ \$(kubectl --context $CONTEXT -n $NS get deploy -l d2k.portainer.io/swarm-managed-by=d2k -o name | wc -l) -ge 3 ]"
elif [ "$SWARM" = false ]; then
	check "running container still running" bash -c '[ "$(docker inspect -f {{.State.Status}} keep)" = running ]'
	check "stopped container shows exited" bash -c '[ "$(docker inspect -f {{.State.Status}} stopme)" = exited ]'
	check "no d2k labels as Docker labels" bash -c "! docker ps -a --format '{{.Labels}}' | grep -q d2k.portainer.io"
	check "compose ls lists the project" bash -c "docker compose ls -a 2>/dev/null | grep -q '^proj '"
	check "compose ps lists its containers" bash -c '[ "$(docker compose -p proj ps -a -q | wc -l)" -eq 2 ]'
	check "volumes listed, labels clean" bash -c "docker volume ls --format '{{.Name}}' | grep -q data-vol && ! docker volume ls --format '{{.Labels}}' | grep -q d2k.portainer.io"
	check "compose up from the original file re-creates with the volume mounted" bash -c "docker compose -p proj -f $WORKDIR/compose.yaml up -d --wait >/dev/null 2>&1 && docker inspect proj-cache-1 -f '{{range .Mounts}}{{.Destination}}{{end}}' | grep -q /data"
	check "docker logs of an old container" docker logs keep
	check "stop and start an old container" bash -c "docker stop keep >/dev/null && docker start keep >/dev/null"
	check "new containers work" bash -c '[ "$(docker run --rm busybox:1.36 echo hi)" = hi ]'
	check "compose down -v cleans up" bash -c "docker compose -p proj -f $WORKDIR/compose.yaml down -v >/dev/null 2>&1 && [ -z \"\$(docker compose -p proj ps -a -q)\" ]"
	check "old containers and volume removable" bash -c "docker rm -f keep stopme >/dev/null && docker volume rm data-vol >/dev/null"
else
	check "the same services listed as before" bash -c "docker service ls --format '{{.Name}}' | sort | diff - $WORKDIR/services"
	check "stack, secret and config listed" bash -c "docker stack ls --format '{{.Name}}' | grep -q stk && docker secret ls --format '{{.Name}}' | grep -q mysecret && docker config ls --format '{{.Name}}' | grep -q myconfig"
	check "service scale" bash -c "docker service scale -d websvc=1 >/dev/null && sleep 10 && docker service ls --format '{{.Name}} {{.Replicas}}' | grep -q 'websvc 1/1'"
	check "stack rm" bash -c "docker stack rm stk >/dev/null && sleep 10 && ! docker service ls --format '{{.Name}}' | grep -q '^stk'"
	check "service, secret and config rm" bash -c "docker service rm websvc >/dev/null && docker secret rm mysecret >/dev/null && docker config rm myconfig >/dev/null"
fi
check "no migration error logged" bash -c "! kubectl --context $CONTEXT -n $NS logs deploy/d2k | grep -q 'Unable to migrate'"
result
