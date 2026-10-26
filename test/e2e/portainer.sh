#!/usr/bin/env bash
# Install the working tree's build as README says (d2k creates its
# certificates), run Portainer CE next to it, add d2k to Portainer as a
# "Docker API" environment over mutual TLS at d2k's in-cluster address, and
# drive d2k through Portainer's API with the calls Portainer's UI makes.
#
#   test/e2e/portainer.sh
#
# PORTAINER_IMAGE picks another Portainer image, CLUSTER another kind cluster.
set -uo pipefail
. "$(dirname "$0")/upgrade-lib.sh"

PORTAINER_IMAGE=${PORTAINER_IMAGE:-portainer/portainer-ce:lts}
NS=e2e-portainer PNS=e2e-portainer-ui PORT=26790

echo "== build the working tree and install it"
image working-tree portainer || { echo "build failed"; exit 1; }
k apply -f "$ROOT/deploy/admission-policy.yaml" >/dev/null
install "$ROOT/deploy/kubernetes.yaml" "$NS" portainer false || { echo "install failed"; exit 1; }
CERTS=$(client_cert "$NS")

echo "== run $PORTAINER_IMAGE"
namespaces+=("$PNS")
k create namespace "$PNS" >/dev/null
k -n "$PNS" create deployment portainer --image="$PORTAINER_IMAGE" --port=9443 >/dev/null
k -n "$PNS" expose deployment portainer --port=9443 >/dev/null
k -n "$PNS" rollout status deploy/portainer --timeout=180s >/dev/null || { echo "Portainer did not start"; exit 1; }

# forward: (re)start the port-forward to Portainer and wait until it answers.
forward() {
	pkill -f "port-forward svc/portainer $PORT:" 2>/dev/null
	k -n "$PNS" port-forward svc/portainer "$PORT:9443" >/dev/null 2>&1 &
	forwards+=($!)
	for _ in $(seq 30); do
		curl -sk -m 3 "https://localhost:$PORT/api/system/status" >/dev/null && return 0
		sleep 1
	done
	return 1
}
forward || { echo "Portainer unreachable"; exit 1; }

# api METHOD PATH [curl args]: call Portainer's API as admin, body in
# $WORKDIR/body, prints the HTTP status. Restarts a dropped port-forward.
api() {
	local m=$1 path=$2 code
	shift 2
	for _ in 1 2; do
		code=$(curl -sk -X "$m" "https://localhost:$PORT/api$path" -H "Authorization: Bearer ${JWT:-}" \
			-o "$WORKDIR/body" -w '%{http_code}' "$@")
		[ "$code" != 000 ] && break
		forward
	done
	echo "$code"
}
# expect CODE METHOD PATH [curl args]: api, and fail unless it answers CODE.
expect() {
	local want=$1
	shift
	[ "$(api "$@")" = "$want" ] || { echo "  $* -> $(api "$@") $(head -c 200 "$WORKDIR/body")" >&2; return 1; }
}
body() { python3 -c "import json,sys; d=json.load(open('$WORKDIR/body')); sys.exit(0 if ($1) else 1)"; }
JSON=(-H Content-Type:application/json)
D=/endpoints/1/docker

echo "== set Portainer up"
PW=$(LC_ALL=C tr -dc A-Za-z0-9 </dev/urandom | head -c 20)
TOKEN=$(k -n "$PNS" logs deploy/portainer | sed -n 's/.*setup_token=\([A-Za-z0-9_-]*\).*/\1/p' | head -1)
check "admin created" expect 200 POST /users/admin/init "${JSON[@]}" -H "X-Setup-Token: $TOKEN" \
	-d "{\"Username\":\"admin\",\"Password\":\"$PW\"}"
api POST /auth "${JSON[@]}" -d "{\"Username\":\"admin\",\"Password\":\"$PW\"}" >/dev/null
JWT=$(python3 -c "import json; print(json.load(open('$WORKDIR/body'))['jwt'])")
check "d2k added as a Docker API environment over mTLS (d2k.$NS.svc)" expect 200 POST /endpoints \
	-F Name=d2k -F EndpointCreationType=1 -F "URL=tcp://d2k.$NS.svc:2376" -F TLS=true -F TLSSkipVerify=false \
	-F TLSSkipClientVerify=false -F "TLSCACertFile=@$CERTS/ca.pem" -F "TLSCertFile=@$CERTS/cert.pem" -F "TLSKeyFile=@$CERTS/key.pem"
check "snapshot" expect 204 POST /endpoints/1/snapshot
check "dashboard" expect 200 GET /docker/1/dashboard
has_d2k_component() { api GET $D/version >/dev/null && body 'any(c["Name"] == "d2k" for c in d["Components"])'; }
check "d2k recognisable by its /version component" has_d2k_component

echo "== containers"
CREATE='{"Image":"nginx:alpine","Env":["FOO=bar"],"Labels":{"team":"qa"},"ExposedPorts":{"80/tcp":{}},
 "HostConfig":{"RestartPolicy":{"Name":"unless-stopped"},"PortBindings":{"80/tcp":[{"HostPort":"8081"}]},
  "Binds":["webdata:/usr/share/nginx/html"],"NetworkMode":"appnet","ShmSize":67108864,"LogConfig":{"Type":"","Config":{}},
  "CapAdd":[],"CapDrop":[],"Devices":[],"DeviceRequests":[],"ExtraHosts":[],"Sysctls":{}},
 "NetworkingConfig":{"EndpointsConfig":{"appnet":{"Aliases":[]}}}}'
check "image pull" expect 200 POST "$D/images/create?fromImage=nginx&tag=alpine" -H 'X-Registry-Auth: e30='
check "volume create" expect 200 POST "$D/volumes/create" "${JSON[@]}" -d '{"Name":"webdata","Driver":"local"}'
check "network create" expect 201 POST "$D/networks/create" "${JSON[@]}" -d '{"Name":"appnet","Driver":"bridge","CheckDuplicate":true}'
check "container create (UI form)" expect 201 POST "$D/containers/create?name=web" "${JSON[@]}" -d "$CREATE"
check "container start" expect 204 POST "$D/containers/web/start"
run_once() {
	expect 201 POST "$D/containers/create?name=once" "${JSON[@]}" \
		-d '{"Image":"busybox:1.36","Cmd":["sh","-c","echo hello from once"],"HostConfig":{"RestartPolicy":{"Name":"no"}}}' &&
		expect 204 POST "$D/containers/once/start"
}
check "run-once container" run_once
sleep 15
listed() { api GET "$D/containers/json?all=1" >/dev/null && body 'sorted(c["Names"][0] for c in d) == ["/once", "/web"] and all(isinstance(c["Ports"], list) for c in d)'; }
check "list: both containers, Ports a list" listed
inspected() { api GET "$D/containers/web/json" >/dev/null && body 'd["State"]["Status"] == "running" and d["Mounts"][0]["Name"] == "webdata" and "appnet" in d["NetworkSettings"]["Networks"]'; }
check "inspect: running with volume and network" inspected
logged() { api GET "$D/containers/once/logs?stdout=1&stderr=1&tail=100" >/dev/null && grep -aq 'hello from once' "$WORKDIR/body"; }
check "logs of the exited run-once container" logged
check "stats" expect 200 GET "$D/containers/web/stats?stream=false"
check "exec create" expect 201 POST "$D/containers/web/exec" "${JSON[@]}" -d '{"AttachStdout":true,"Tty":true,"Cmd":["sh"]}'
check "events" expect 200 GET "$D/events?since=$(($(date +%s) - 600))&until=$(date +%s)"
check "stop" expect 204 POST "$D/containers/web/stop"
check "start" expect 204 POST "$D/containers/web/start"
check "restart" expect 204 POST "$D/containers/web/restart"
check "rename" expect 204 POST "$D/containers/web/rename?name=web2"
# Portainer hides these for d2k; d2k answers them honestly.
for call in "POST $D/containers/web2/kill" "POST $D/containers/web2/pause" "GET $D/containers/web2/top" "GET $D/system/df"; do
	# shellcheck disable=SC2086 # method and path
	check "unsupported, 501: $call" expect 501 $call
done
recreate() {
	expect 204 POST "$D/containers/web2/rename?name=web2-old" && expect 204 POST "$D/containers/web2-old/stop" &&
		expect 201 POST "$D/containers/create?name=web2" "${JSON[@]}" -d "$CREATE" &&
		expect 204 POST "$D/containers/web2/start" && expect 204 DELETE "$D/containers/web2-old?force=true"
}
check "recreate (UI): rename, create, remove old" recreate

echo "== volumes and networks"
check "volume list" expect 200 GET "$D/volumes"
check "volume in use: 409" expect 409 DELETE "$D/volumes/webdata"
check "network inspect" expect 200 GET "$D/networks/appnet"
check "network connect" expect 200 POST "$D/networks/appnet/connect" "${JSON[@]}" -d '{"Container":"once"}'
check "network disconnect" expect 200 POST "$D/networks/appnet/disconnect" "${JSON[@]}" -d '{"Container":"once"}'

echo "== stacks (Portainer's compose library)"
STACK=$(python3 -c 'import json; print(json.dumps({"Name": "shop", "Env": [{"name": "GREETING", "value": "hi"}], "StackFileContent": """services:
  web:
    image: nginx:alpine
    ports: ["8090:80"]
    depends_on: [cache]
    environment:
      GREETING: ${GREETING}
  cache:
    image: redis:7-alpine
    volumes: [cachedata:/data]
  init:
    image: busybox:1.36
    command: ["sh", "-c", "echo init done"]
    restart: "no"
volumes:
  cachedata:
"""}))')
SHOP="$D/containers/json?all=1&filters=%7B%22label%22%3A%5B%22com.docker.compose.project%3Dshop%22%5D%7D"
check "stack deploy" expect 200 POST "/stacks/create/standalone/string?endpointId=1" "${JSON[@]}" -d "$STACK"
sleep 20
stack_up() { api GET "$SHOP" >/dev/null && body 'sorted((c["Names"][0], c["State"]) for c in d) == [("/shop-cache-1", "running"), ("/shop-init-1", "exited"), ("/shop-web-1", "running")]'; }
check "stack containers up" stack_up
check "stack stop" expect 200 POST "/stacks/1/stop?endpointId=1"
check "stack start" expect 200 POST "/stacks/1/start?endpointId=1"
UPDATE=$(python3 -c 'import json, sys; s = json.loads(sys.argv[1]); print(json.dumps({"StackFileContent": s["StackFileContent"].replace("8090:80", "8091:80"), "Env": s["Env"], "Prune": True}))' "$STACK")
check "stack update" expect 200 PUT "/stacks/1?endpointId=1" "${JSON[@]}" -d "$UPDATE"
sleep 15
new_port() { api GET "$SHOP" >/dev/null && body 'any(p.get("PublicPort") == 8091 for c in d for p in c["Ports"])'; }
check "stack web on its new port" new_port
check "stack delete" expect 204 DELETE "/stacks/1?endpointId=1"
sleep 10
stack_gone() { api GET "$SHOP" >/dev/null && body 'd == []'; }
check "stack containers gone" stack_gone

echo "== clean up"
remove_containers() { expect 204 DELETE "$D/containers/web2?force=true" && expect 204 DELETE "$D/containers/once?force=true"; }
check "remove containers" remove_containers
sleep 5
check "remove volume" expect 204 DELETE "$D/volumes/webdata"
check "remove network" expect 204 DELETE "$D/networks/appnet"
result
