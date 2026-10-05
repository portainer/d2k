#!/usr/bin/env bash
# End-to-end tests against the two d2k instances deployed by setup.sh, driven
# through the real Docker CLI and Docker Compose, with cluster state checked
# through kubectl.
#
# Results: PASS / FAIL, plus XFAIL for known gaps (tagged with the phase that
# fixes them) and XPASS when a known gap starts working (promote the case).
# Exits non-zero on any FAIL or XPASS.
set -uo pipefail

CONTEXT=kind-${CLUSTER:-d2k}
STANDALONE=tcp://localhost:31112
SWARM=tcp://localhost:31111
WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT

# d2k serves mutual TLS: the Docker CLI, Compose and curl use the client
# certificate created by setup.sh.
PKI=$(cd "$(dirname "$0")" && pwd)/.pki/client
export DOCKER_TLS_VERIFY=1 DOCKER_CERT_PATH=$PKI CURL_HOME=$WORKDIR
printf 'cacert = "%s/ca.pem"\ncert = "%s/cert.pem"\nkey = "%s/key.pem"\n' "$PKI" "$PKI" "$PKI" >"$WORKDIR/.curlrc"

pass=0 fail=0 xfail=0 xpass=0
failed=()

k() { kubectl --context "$CONTEXT" "$@"; }

# to <seconds> <cmd...>: run cmd with a timeout (macOS has no coreutils timeout).
to() { perl -e 'alarm shift; exec @ARGV' "$@"; }

# eventually <seconds> <cmd...>: retry cmd every 2s until it succeeds.
eventually() {
  local deadline=$((SECONDS + $1)); shift
  until "$@" >/dev/null 2>&1; do
    ((SECONDS >= deadline)) && return 1
    sleep 2
  done
}

# check <name> <cmd...>: the case passes when cmd exits 0.
check() {
  local name=$1; shift
  if ("$@") >"$WORKDIR/out" 2>&1; then
    echo "PASS   $name"; pass=$((pass + 1))
  else
    echo "FAIL   $name"; sed 's/^/         /' "$WORKDIR/out" | tail -5
    fail=$((fail + 1)); failed+=("$name")
  fi
}

# known <phase> <name> <cmd...>: a known gap, expected to fail until <phase>.
known() {
  local phase=$1 name=$2; shift 2
  if ("$@") >"$WORKDIR/out" 2>&1; then
    echo "XPASS  $name (expected to fail until $phase)"; xpass=$((xpass + 1)); failed+=("$name")
  else
    echo "XFAIL  $name ($phase)"; xfail=$((xfail + 1))
  fi
}

# reset removes every object d2k created in both namespaces.
reset() {
  for ns in d2k d2k-swarm; do
    k -n "$ns" delete deploy,svc,pvc,secret,cm -l d2k.portainer.io/managed-by=d2k --wait=false >/dev/null 2>&1
  done
  eventually 120 bash -c "[ -z \"\$(kubectl --context $CONTEXT get deploy -A -l d2k.portainer.io/managed-by=d2k -o name)\" ]"
}

replicas() { k -n "$1" get deploy "$2" -o jsonpath='{.spec.replicas}'; }

# ---------------------------------------------------------------- standalone

standalone_cases() {
  export DOCKER_HOST=$STANDALONE
  echo "== standalone ($STANDALONE)"

  check "version negotiates" docker version
  check "info identifies the namespace" bash -c "[ \"\$(docker info -f {{.ID}})\" = \"\$(kubectl --context $CONTEXT get ns d2k -o jsonpath={.metadata.uid})\" ]"
  check "info reports cluster CPUs" bash -c '[ "$(docker info -f {{.NCPU}})" -gt 0 ]'
  check "d2k runs unprivileged" bash -c "[ \"\$(kubectl --context $CONTEXT -n d2k get pod -l app=d2k -o jsonpath='{.items[0].spec.securityContext.runAsNonRoot} {.items[0].spec.containers[0].securityContext.readOnlyRootFilesystem}')\" = 'true true' ]"
  # Mutual TLS is required.
  check "plaintext is refused" bash -c '! curl -sf -m 5 http://localhost:31112/_ping'
  check "TLS without a client certificate is refused" bash -c "! curl -q -sf -m 5 --cacert $PKI/ca.pem https://localhost:31112/_ping"
  check "TLS with a client certificate is accepted" bash -c '[ "$(curl -s https://localhost:31112/_ping)" = OK ]'

  check "run starts a container" bash -c '
    docker run -d --name e2e-web nginx:alpine >/dev/null &&
    for _ in $(seq 60); do [ "$(docker inspect -f {{.State.Status}} e2e-web)" = running ] && exit 0; sleep 2; done; exit 1'
  check "container IDs are 64 hex characters" bash -c '[[ "$(docker ps -q --no-trunc --filter name=e2e-web)" =~ ^[0-9a-f]{64}$ ]]'
  check "ps lists the container" bash -c 'docker ps --format {{.Names}} | grep -qx e2e-web'
  check "logs returns output" bash -c 'docker logs e2e-web 2>&1 | grep -q "start worker"'
  check "logs --tail 0 returns nothing" bash -c '[ -z "$(docker logs --tail 0 e2e-web 2>&1)" ]'
  check "logs --tail 1 returns one line" bash -c '[ "$(docker logs --tail 1 e2e-web 2>&1 | wc -l | tr -d " ")" = 1 ]'
  check "logs -t adds timestamps" bash -c 'docker logs -t --tail 1 e2e-web 2>&1 | grep -qE "^20[0-9]{2}-[0-9]{2}-[0-9]{2}T"'
  check "logs --until in the past returns nothing" bash -c '[ -z "$(docker logs --until 2000-01-01T00:00:00Z e2e-web 2>&1)" ]'
  check "logs --since now-1h returns lines" bash -c '[ -n "$(docker logs --since 1h e2e-web 2>&1)" ]'
  check "logs of a TTY container" bash -c '
    docker run -d -t --name e2e-ttylog nginx:alpine sh -c "echo tty-line; sleep 600" >/dev/null &&
    for _ in $(seq 60); do docker logs e2e-ttylog 2>&1 | grep -q tty-line && exit 0; sleep 2; done; exit 1'
  docker rm -f e2e-ttylog >/dev/null 2>&1
  check "stop scales to 0" bash -c "docker stop e2e-web >/dev/null && [ \"\$(kubectl --context $CONTEXT -n d2k get deploy e2e-web -o jsonpath={.spec.replicas})\" = 0 ]"
  check "start scales to 1" bash -c "docker start e2e-web >/dev/null && [ \"\$(kubectl --context $CONTEXT -n d2k get deploy e2e-web -o jsonpath={.spec.replicas})\" = 1 ]"
  eventually 60 bash -c '[ "$(docker inspect -f {{.State.Status}} e2e-web)" = running ]'
  check "exec returns output" bash -c '[ "$(perl -e "alarm 20; exec @ARGV" docker exec e2e-web echo hi)" = hi ]'
  check "exec with a TTY returns output" bash -c 'perl -e "alarm 20; exec @ARGV" docker exec -t e2e-web echo hi | grep -q hi'
  check "exec returns the exit code" bash -c 'docker exec e2e-web sh -c "exit 3"; [ $? = 3 ]'
  check "exec passes stdin" bash -c '[ "$(echo hello | perl -e "alarm 20; exec @ARGV" docker exec -i e2e-web cat)" = hello ]'
  check "exec keeps stderr separate" bash -c '[ -z "$(docker exec e2e-web sh -c "echo err >&2" 2>/dev/null)" ] && [ "$(docker exec e2e-web sh -c "echo err >&2" 2>&1 >/dev/null)" = err ]'
  # The resize reaches the process asynchronously, hence the sleep.
  check "exec TTY gets the client's terminal size" bash -c "python3 $(dirname "$0")/run_in_terminal.py docker exec -it e2e-web sh -c 'sleep 1; ttysize' | grep -q '120 40'"
  check "exec as another user is refused" bash -c 'docker exec -u root e2e-web id 2>&1 | grep -q "not supported by d2k"'

  # Docker lifecycle states: create does not start.
  docker create --name e2e-life nginx:alpine >/dev/null
  check "create leaves the container created" bash -c '[ "$(docker inspect -f {{.State.Status}} e2e-life)" = created ]'
  check "start runs it" bash -c 'docker start e2e-life >/dev/null && for _ in $(seq 60); do [ "$(docker inspect -f {{.State.Status}} e2e-life)" = running ] && exit 0; sleep 2; done; exit 1'
  check "stop exits it" bash -c 'docker stop e2e-life >/dev/null && [ "$(docker inspect -f {{.State.Status}} e2e-life)" = exited ] && docker ps -a --filter name=e2e-life --format {{.Status}} | grep -q "^Exited (0)"'
  check "ps without -a hides stopped containers" bash -c '! docker ps --format {{.Names}} | grep -qx e2e-life'
  docker rm e2e-life >/dev/null

  # docker run settings are translated, and unsupported ones reported.
  check "run settings reach the container" bash -c "
    docker run -d --name e2e-settings -w /tmp -u 101:101 -m 64m --cpus 0.5 --add-host db:10.0.0.5 nginx:alpine sleep 600 >/dev/null &&
    for _ in \$(seq 60); do [ \"\$(docker inspect -f {{.State.Status}} e2e-settings)\" = running ] && break; sleep 2; done &&
    [ \"\$(kubectl --context $CONTEXT -n d2k exec deploy/e2e-settings -- sh -c 'pwd; id -u; grep db /etc/hosts' | tr -s ' \t\n' ' ')\" = '/tmp 101 10.0.0.5 db ' ] &&
    [ \"\$(kubectl --context $CONTEXT -n d2k get deploy e2e-settings -o jsonpath='{.spec.template.spec.containers[0].resources.limits}')\" = '{\"cpu\":\"500m\",\"memory\":\"64Mi\"}' ]"
  check "plain create reports no warnings" bash -c '[ -z "$(docker create --name e2e-quiet nginx:alpine 2>&1 >/dev/null)" ]'
  docker rm e2e-quiet >/dev/null 2>&1
  check "unsupported settings are reported" bash -c 'docker create --name e2e-warn --dns 1.1.1.1 nginx:alpine 2>&1 >/dev/null | grep -q "Dns is not supported by d2k"'
  docker rm -f e2e-settings e2e-warn >/dev/null 2>&1

  # Events are recorded and replayed with since/until, and streamed live.
  check "events replay a container's lifecycle" bash -c '
    start=$(date +%s); docker create --name e2e-ev nginx:alpine >/dev/null && docker start e2e-ev >/dev/null &&
    docker stop e2e-ev >/dev/null && docker rm e2e-ev >/dev/null && sleep 2 &&
    [ "$(docker events --since $start --until $(( $(date +%s) + 1 )) --filter container=e2e-ev --format "{{.Action}}" | tr "\n" " ")" = "create start stop destroy " ]'
  check "events stream live" bash -c '
    (perl -e "alarm 20; exec @ARGV" docker events --filter type=volume --format "{{.Action}} {{.Actor.ID}}" > '"$WORKDIR"'/events.out 2>/dev/null &) ;
    sleep 3; docker volume create e2e-ev-vol >/dev/null; sleep 3; docker volume rm e2e-ev-vol >/dev/null;
    for _ in $(seq 10); do grep -q "destroy e2e-ev-vol" '"$WORKDIR"'/events.out && grep -q "create e2e-ev-vol" '"$WORKDIR"'/events.out && exit 0; sleep 1; done; exit 1'

  # docker run without -d: create, wait, attach, start (busybox has no entrypoint).
  check "run prints the output" bash -c '[ "$(perl -e "alarm 120; exec @ARGV" docker run --name e2e-fg busybox:1.36 echo hi)" = hi ]'
  check "run returns the exit code" bash -c 'perl -e "alarm 120; exec @ARGV" docker run --name e2e-code busybox:1.36 sh -c "exit 3"; [ $? = 3 ]'
  check "run -i passes stdin" bash -c '[ "$(echo hello | perl -e "alarm 120; exec @ARGV" docker run -i --name e2e-in busybox:1.36 cat)" = hello ]'
  check "run -it attaches a TTY of the client's size" bash -c "python3 $(dirname "$0")/run_in_terminal.py docker run -it --name e2e-it busybox:1.36 sh -c 'sleep 2; ttysize' | grep -q '120 40'"
  check "wait returns when the container stops" bash -c '
    docker run -d --name e2e-wait nginx:alpine >/dev/null &&
    for _ in $(seq 60); do [ "$(docker inspect -f {{.State.Status}} e2e-wait)" = running ] && break; sleep 2; done &&
    (perl -e "alarm 60; exec @ARGV" docker wait e2e-wait > '"$WORKDIR"'/wait.out &) ; sleep 2; docker stop e2e-wait >/dev/null;
    for _ in $(seq 20); do [ "$(cat '"$WORKDIR"'/wait.out)" = 0 ] && exit 0; sleep 1; done; exit 1'
  docker rm -f e2e-fg e2e-code e2e-in e2e-it e2e-wait >/dev/null 2>&1

  # Stats come from metrics-server (installed by setup.sh); its first sample
  # for a new pod takes up to a minute.
  docker run -d --name e2e-busy -m 64m busybox:1.36 sh -c 'while true; do :; done' >/dev/null
  check "stats report a busy container's CPU" bash -c '
    for _ in $(seq 45); do
      cpu=$(docker stats --no-stream --format "{{.CPUPerc}}" e2e-busy | tr -d "%" | cut -d. -f1)
      [ "${cpu:-0}" -ge 50 ] && exit 0; sleep 4
    done; exit 1'
  check "stats report the memory limit" bash -c 'docker stats --no-stream --format "{{.MemUsage}}" e2e-busy | grep -q "/ 64MiB"'
  check "stats stream one sample a second" bash -c '[ "$(perl -e "alarm 4; exec @ARGV" curl -s -N "https://localhost:31112/v1.52/containers/e2e-busy/stats" | wc -l | tr -d " ")" -ge 3 ]'
  docker rm -f e2e-busy >/dev/null 2>&1

  # Phase 0: name resolution.
  docker run -d --name e2e-my-api nginx:alpine >/dev/null
  check "rm does not match by name suffix" bash -c '! docker rm api 2>/dev/null && docker container inspect e2e-my-api >/dev/null'
  check "d2k cannot act on its own Deployment" bash -c "! docker stop d2k 2>/dev/null && [ \"\$(kubectl --context $CONTEXT -n d2k get deploy d2k -o jsonpath={.spec.replicas})\" = 1 ]"

  # Phase 0: list filters.
  docker run -d --name e2e-group-a --label e2e.group=a nginx:alpine >/dev/null
  docker run -d --name e2e-group-b --label e2e.group=b nginx:alpine >/dev/null
  check "ps honours label filter" bash -c '[ "$(docker ps -a --filter label=e2e.group=a --format {{.Names}})" = e2e-group-a ]'
  check "ps honours name filter" bash -c '[ "$(docker ps -a --filter name=e2e-group-b --format {{.Names}})" = e2e-group-b ]'
  check "ps rejects unsupported filter" bash -c 'docker ps --filter ancestor=nginx 2>&1 | grep -q "invalid filter"'

  # Compose (what Portainer stacks use).
  mkdir -p "$WORKDIR/compose" && printf 'services:\n  web:\n    image: nginx:alpine\n' >"$WORKDIR/compose/compose.yaml"
  check "compose up" bash -c "cd $WORKDIR/compose && to() { perl -e 'alarm shift; exec @ARGV' \"\$@\"; } && to 120 docker compose -p e2ea up -d && to 120 docker compose -p e2eb up -d"
  check "compose down leaves other projects alone" bash -c "cd $WORKDIR/compose && perl -e 'alarm 60; exec @ARGV' docker compose -p e2ea down && docker inspect e2eb-web-1 >/dev/null"
  check "compose down removes its own project" bash -c "cd $WORKDIR/compose && perl -e 'alarm 60; exec @ARGV' docker compose -p e2eb down && ! docker container inspect e2eb-web-1 >/dev/null 2>&1"

  # Volumes, including unversioned paths (Portainer UI).
  check "volume create and label filter" bash -c 'docker volume create --label e2e=1 e2e-vol >/dev/null && [ "$(docker volume ls -q --filter label=e2e=1)" = e2e-vol ]'
  check "unversioned /volumes/create routes" bash -c '[ "$(curl -s -o /dev/null -w %{http_code} -X POST -H content-type:application/json -d "{\"Name\":\"e2e-vol-raw\"}" https://localhost:31112/volumes/create)" = 201 ]'
  check "unversioned /volumes/{name} routes" bash -c '[ "$(curl -s -o /dev/null -w %{http_code} https://localhost:31112/volumes/e2e-vol-raw)" = 200 ]'
  check "volume keeps its name, labels and options" bash -c '
    long=$(printf "x%.0s" $(seq 80)) &&
    docker volume create --label "long=$long" --opt size=2Gi e2e_vol_u >/dev/null &&
    [ "$(docker volume inspect -f "{{.Name}} {{.Scope}} {{index .Labels \"long\"}} {{.Options.size}}" e2e_vol_u)" = "e2e_vol_u local $long 2Gi" ]'
  check "creating an existing volume returns it" bash -c '[ "$(docker volume create e2e_vol_u)" = e2e_vol_u ]'
  check "volume rm" bash -c 'docker volume rm e2e-vol e2e-vol-raw e2e_vol_u >/dev/null'

  # Images: d2k stores none; the kubelet pulls them.
  check "docker pull is acknowledged" bash -c 'docker pull nginx:alpine 2>&1 | grep -q "the kubelet pulls it"'
  check "docker images lists none" bash -c '[ -z "$(docker images -q)" ]'
  check "image inspect reports the reference only" bash -c '[ "$(docker image inspect -f "{{.RepoTags}} {{.Config}}" nginx:alpine)" = "[nginx:alpine] <nil>" ]'
  check "docker rmi is not supported" bash -c 'docker rmi nginx:alpine 2>&1 | grep -q "not supported by d2k"'

  # Docker Engine API operations d2k does not implement answer 501.
  check "unsupported operation returns 501" bash -c '[ "$(curl -s -o /dev/null -w %{http_code} https://localhost:31112/v1.52/system/df)" = 501 ]'
  check "docker reports an unsupported operation" bash -c 'docker system df 2>&1 | grep -q "not supported by d2k"'
  # Status codes clients branch on (compose treats 404 as "gone").
  check "removing a missing container returns 404" bash -c '[ "$(curl -s -o /dev/null -w %{http_code} -X DELETE https://localhost:31112/v1.52/containers/e2e-missing)" = 404 ]'
  check "duplicate container name returns 409" bash -c '[ "$(curl -s -o /dev/null -w %{http_code} -X POST -H content-type:application/json -d "{\"Image\":\"nginx:alpine\"}" "https://localhost:31112/v1.52/containers/create?name=e2e-group-a")" = 409 ]'
  check "API versions older than 1.44 are rejected" bash -c 'curl -s https://localhost:31112/v1.41/_ping | grep -q "client version 1.41 is too old"'
  check "non-Docker path returns 404" bash -c '[ "$(curl -s -o /dev/null -w %{http_code} https://localhost:31112/v1.52/not/docker)" = 404 ]'

  check "rm deletes the Deployment" bash -c "docker rm -f e2e-web >/dev/null && ! kubectl --context $CONTEXT -n d2k get deploy e2e-web >/dev/null 2>&1"
}

# ------------------------------------------------------------------- compose

# compose_cases runs a realistic project through its whole lifecycle: a
# published port, a named volume, two networks and a command override.
compose_cases() {
  export DOCKER_HOST=$STANDALONE
  echo "== compose lifecycle ($STANDALONE)"
  COMPOSE_DIR=$WORKDIR/lifecycle
  mkdir -p "$COMPOSE_DIR"
  cat >"$COMPOSE_DIR/compose.yaml" <<'YAML'
services:
  web:
    image: nginx:alpine
    ports: ["18080:80"]
    environment: [MODE=one]
    volumes: [data:/data]
    networks: [front, back]
  cache:
    image: redis:7-alpine
    command: ["redis-server", "--save", ""]
    networks: [back]
volumes:
  data:
networks:
  front:
  back:
YAML

  check "compose up (port, volume, networks)" dc up -d
  eventually 120 running e2ec-web-1
  eventually 120 running e2ec-cache-1
  check "compose ps lists the project" compose_count_is 2
  check "compose up again changes nothing" up_keeps_ids
  check "command keeps the image entrypoint" cache_keeps_entrypoint
  check "services resolve each other by service name" in_service web getent hosts cache
  check "service names stay within their project" names_stay_in_project
  check "published port gets a Service" published_port_exists 18080
  check "compose volume keeps its project name" bash -c '[ "$(docker volume ls -q --filter label=com.docker.compose.project=e2ec)" = e2ec_data ]'
  check "named volume is mounted" in_service web sh -c 'echo kept > /data/marker'
  check "compose logs" logs_show_ready

  sed -i.bak 's/MODE=one/MODE=two/' "$COMPOSE_DIR/compose.yaml"
  check "compose up recreates a changed service" up_recreates_web
  eventually 120 running e2ec-web-1
  check "volume data survives recreate" marker_kept
  check "compose stop" stop_all
  check "compose start" start_all
  check "compose down -v removes everything" down_removes_everything
}

# health_cases checks healthchecks: compose waits on service_healthy, and a
# failing check makes a container unhealthy while it keeps running.
health_cases() {
  export DOCKER_HOST=$STANDALONE
  echo "== healthchecks ($STANDALONE)"
  local dir=$WORKDIR/health
  mkdir -p "$dir"
  cat >"$dir/compose.yaml" <<'YAML'
services:
  db:
    image: redis:7-alpine
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 2s
      timeout: 2s
      retries: 5
  app:
    image: nginx:alpine
    depends_on:
      db:
        condition: service_healthy
YAML
  check "compose waits for service_healthy" bash -c "cd $dir && perl -e 'alarm 180; exec @ARGV' docker compose -p e2eh up -d"
  check "healthy container reports healthy" bash -c '[ "$(docker inspect -f {{.State.Health.Status}} e2eh-db-1)" = healthy ] && docker ps --filter name=e2eh-db-1 --format {{.Status}} | grep -q "(healthy)"'
  check "failing healthcheck reports unhealthy" bash -c '
    docker run -d --name e2e-sick --health-cmd false --health-interval 1s --health-retries 1 nginx:alpine >/dev/null &&
    for _ in $(seq 60); do [ "$(docker inspect -f {{.State.Health.Status}} e2e-sick 2>/dev/null)" = unhealthy ] && break; sleep 2; done &&
    [ "$(docker inspect -f "{{.State.Status}} {{.State.Health.Status}}" e2e-sick)" = "running unhealthy" ]'
  docker rm -f e2e-sick >/dev/null 2>&1
  (cd "$dir" && to 120 docker compose -p e2eh down) >/dev/null 2>&1
}

# dc <args...>: docker compose for the e2ec lifecycle project, with a timeout.
dc() { (cd "$COMPOSE_DIR" && to 180 docker compose -p e2ec "$@"); }
running() { [ "$(docker inspect -f '{{.State.Status}}' "$1" 2>/dev/null)" = running ]; }
compose_count_is() { [ "$(dc ps -q | wc -l | tr -d ' ')" = "$1" ]; }
# in_service <compose service> <cmd...>: run cmd in the service's pod via kubectl.
in_service() {
  local deploy
  deploy=$(k -n d2k get deploy -l "com.docker.compose.project=e2ec,com.docker.compose.service=$1" -o jsonpath='{.items[0].metadata.name}')
  k -n d2k exec "deploy/$deploy" -- "${@:2}"
}
up_keeps_ids() {
  local before
  before=$(docker inspect -f '{{.Id}}' e2ec-web-1 e2ec-cache-1) && dc up -d &&
    [ "$before" = "$(docker inspect -f '{{.Id}}' e2ec-web-1 e2ec-cache-1)" ]
}
up_recreates_web() {
  local before
  before=$(docker inspect -f '{{.Id}}' e2ec-web-1) && dc up -d &&
    [ "$before" != "$(docker inspect -f '{{.Id}}' e2ec-web-1)" ] &&
    docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' e2ec-web-1 | grep -qx MODE=two
}
cache_keeps_entrypoint() {
  local c=".items[0].spec.template.spec.containers[0]"
  [ -z "$(k -n d2k get deploy -l com.docker.compose.service=cache -o jsonpath="{$c.command}")" ] &&
    k -n d2k get deploy -l com.docker.compose.service=cache -o jsonpath="{$c.args}" | grep -q redis-server
}
logs_show_ready() { dc logs cache 2>&1 | grep -q 'Ready to accept connections'; }
marker_kept() { [ "$(in_service web cat /data/marker)" = kept ]; }
stop_all() { dc stop && compose_count_is 0; }
start_all() { dc start && eventually 120 compose_count_is 2; }
# names_stay_in_project deploys a second project with the same service names
# and checks that "cache" still resolves to the first project's container.
names_stay_in_project() {
  local other=$WORKDIR/other ip
  mkdir -p "$other" && sed 's/18080:80/18081:80/' "$COMPOSE_DIR/compose.yaml" >"$other/compose.yaml"
  (cd "$other" && to 180 docker compose -p e2ed up -d) || return 1
  eventually 120 running e2ed-cache-1
  local deploy resolved
  deploy=$(k -n d2k get deploy -l com.docker.compose.project=e2ec,com.docker.compose.service=cache -o jsonpath='{.items[0].metadata.name}')
  ip=$(k -n d2k get pod -l "app=$deploy" -o jsonpath='{.items[0].status.podIP}')
  resolved=$(in_service web getent hosts cache | awk '{print $1}')
  (cd "$other" && to 120 docker compose -p e2ed down -v) >/dev/null
  [ -n "$ip" ] && [ "$resolved" = "$ip" ]
}
published_port_exists() { k -n d2k get svc -o jsonpath='{.items[*].spec.ports[*].port}' | grep -qw "$1"; }
no_project_objects() { ! k -n d2k get deploy,svc,pvc -o name | grep -q e2ec; }
down_removes_everything() { dc down -v && eventually 120 no_project_objects; }

# --------------------------------------------------------------------- swarm

swarm_cases() {
  export DOCKER_HOST=$SWARM
  echo "== swarm ($SWARM)"

  check "info reports swarm active" bash -c '[ "$(docker info -f {{.Swarm.LocalNodeState}})" = active ]'
  check "service create converges" bash -c "perl -e 'alarm 120; exec @ARGV' docker service create --name e2e-svc nginx:alpine"
  check "service scale" bash -c "perl -e 'alarm 120; exec @ARGV' docker service scale e2e-svc=2 && [ \"\$(kubectl --context $CONTEXT -n d2k-swarm get deploy e2e-svc -o jsonpath={.spec.replicas})\" = 2 ]"
  check "service rm" bash -c "docker service rm e2e-svc >/dev/null && ! kubectl --context $CONTEXT -n d2k-swarm get deploy e2e-svc >/dev/null 2>&1"

  # Phase 0: service delete must not guess Service names ("e2e-d2k" -> "d2k").
  check "service rm keeps d2k's own Service" bash -c "
    perl -e 'alarm 120; exec @ARGV' docker service create --detach --name e2e-d2k nginx:alpine >/dev/null &&
    docker service rm e2e-d2k >/dev/null &&
    kubectl --context $CONTEXT -n d2k-swarm get svc d2k >/dev/null &&
    ! kubectl --context $CONTEXT -n d2k-swarm get svc e2e-d2k >/dev/null 2>&1"
  check "service create cannot overwrite d2k" bash -c "
    ! docker service create --detach --name d2k busybox 2>/dev/null &&
    [ \"\$(kubectl --context $CONTEXT -n d2k-swarm get deploy d2k -o jsonpath='{.spec.template.spec.containers[0].image}')\" = d2k:dev ]"

  # Stacks, secrets and DNS names.
  mkdir -p "$WORKDIR/stack" && printf 's3cret\n' >"$WORKDIR/stack/pw.txt"
  cat >"$WORKDIR/stack/stack.yaml" <<'YAML'
services:
  redis:
    image: redis:7-alpine
    secrets: [db_password]
secrets:
  db_password:
    file: ./pw.txt
YAML
  check "stack deploy" bash -c "cd $WORKDIR/stack && perl -e 'alarm 120; exec @ARGV' docker stack deploy -c stack.yaml --detach=true a"
  check "second stack with same service name is rejected" bash -c "cd $WORKDIR/stack && perl -e 'alarm 120; exec @ARGV' docker stack deploy -c stack.yaml --detach=true b 2>&1 | grep -q 'DNS name \"redis\" is already used'"
  check "rejected stack leaves first stack's DNS intact" bash -c "[ \"\$(kubectl --context $CONTEXT -n d2k-swarm get svc redis -o jsonpath='{.metadata.labels.d2k\\.portainer\\.io/swarm-service}')\" = a-redis ]"
  check "stack rm removes only its own secrets" bash -c "
    perl -e 'alarm 60; exec @ARGV' docker stack rm b >/dev/null && sleep 2 &&
    docker secret ls --format {{.Name}} | grep -qx a_db_password &&
    ! docker secret ls --format {{.Name}} | grep -qx b_db_password"
  check "stack redeploy updates in place" bash -c "cd $WORKDIR/stack && perl -e 'alarm 120; exec @ARGV' docker stack deploy -c stack.yaml --detach=true a"
  known "Swarm phase" "stack redeploy recognises existing services" bash -c "cd $WORKDIR/stack && perl -e 'alarm 120; exec @ARGV' docker stack deploy -c stack.yaml --detach=true a | grep -q 'Updating service a_redis'"

  # Phase 0: only Swarm secrets are mountable.
  k -n d2k-swarm create secret generic e2e-unmanaged --from-literal=k=v >/dev/null
  check "service cannot mount an unmanaged secret" bash -c "[ \"\$(curl -s -o /dev/null -w %{http_code} -X POST -H content-type:application/json \
    -d '{\"Name\":\"e2e-leak\",\"TaskTemplate\":{\"ContainerSpec\":{\"Image\":\"busybox\",\"Secrets\":[{\"SecretName\":\"e2e-unmanaged\",\"File\":{\"Name\":\"k\"}}]}}}' \
    https://localhost:31111/services/create)\" = 400 ]"
  k -n d2k-swarm delete secret e2e-unmanaged >/dev/null

  check "config create and label filter" bash -c "printf 'x' | docker config create --label e2e=1 e2e_conf - >/dev/null && [ \"\$(docker config ls --filter label=e2e=1 --format {{.Name}})\" = e2e_conf ]"
  check "stack rm" bash -c "perl -e 'alarm 60; exec @ARGV' docker stack rm a >/dev/null && sleep 2 && ! kubectl --context $CONTEXT -n d2k-swarm get deploy a-redis >/dev/null 2>&1"
}

reset
standalone_cases
compose_cases
health_cases
swarm_cases
reset

echo
echo "passed: $pass  failed: $fail  known gaps: $xfail  unexpectedly passing: $xpass"
for name in ${failed[@]+"${failed[@]}"}; do echo "  - $name"; done
[ "$fail" -eq 0 ] && [ "$xpass" -eq 0 ]
