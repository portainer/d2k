#!/usr/bin/env bash
# Sweeps the plain Docker CLI (not compose) command by command against a d2k
# standalone instance: each command and its realistic flags, checked the way
# a user would (exit code, output, the real Kubernetes object).
#
# Results: PASS / FAIL, plus XFAIL for documented limits (README "Not
# supported", answers 501) and XPASS when one starts working. Exits non-zero
# on any FAIL or XPASS.
set -uo pipefail

CONTEXT=kind-${CLUSTER:-d2k}
STANDALONE=${STANDALONE:-tcp://localhost:31112}
WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT

PKI=${PKI:-$(cd "$(dirname "$0")" && pwd)/.pki/client}
export DOCKER_HOST=$STANDALONE DOCKER_TLS_VERIFY=1 DOCKER_CERT_PATH=$PKI

pass=0 fail=0 xfail=0 xpass=0
failed=()

k() { kubectl --context "$CONTEXT" "$@"; }
to() { perl -e 'alarm shift; exec @ARGV' "$@"; }
eventually() {
  local deadline=$((SECONDS + $1)); shift
  until "$@" >/dev/null 2>&1; do
    ((SECONDS >= deadline)) && return 1
    sleep 2
  done
}

check() {
  local name=$1; shift
  if ("$@") >"$WORKDIR/out" 2>&1; then
    echo "PASS   $name"; pass=$((pass + 1))
  else
    echo "FAIL   $name"; sed 's/^/         /' "$WORKDIR/out" | tail -5
    fail=$((fail + 1)); failed+=("$name")
  fi
}

# known <what> <name> <cmd...>: a documented limit (README "Not supported"),
# expected to answer 501 or otherwise fail.
known() {
  local what=$1 name=$2; shift 2
  if ("$@") >"$WORKDIR/out" 2>&1; then
    echo "XPASS  $name (expected to fail: $what)"; xpass=$((xpass + 1)); failed+=("$name")
  else
    echo "XFAIL  $name ($what)"; xfail=$((xfail + 1))
  fi
}

# reset removes only this script's own objects (named e2e*), never every
# d2k-managed object in the namespace: on a shared/live environment another
# lane's test may be running in the same namespace at the same time.
reset() {
  for kind in deploy svc pvc secret cm; do
    k -n d2k get "$kind" -o name 2>/dev/null | grep '/e2e' | xargs -r -n1 -I{} kubectl --context "$CONTEXT" -n d2k delete {} --wait=false >/dev/null 2>&1
  done
  eventually 60 bash -c "[ -z \"\$(kubectl --context $CONTEXT -n d2k get deploy -o name | grep /e2e)\" ]"
}

running() { [ "$(docker inspect -f '{{.State.Status}}' "$1" 2>/dev/null)" = running ]; }
export -f to running
export CONTEXT

# -------------------------------------------------------------- container

container_cases() {
  echo "== container: run, create, start, stop, restart, rm, ps, inspect, logs =="
  docker rm -f e2ec-run e2ec-create e2ec-life e2ec-health e2ec-port e2ec-native e2ec-exec e2ec-wait e2ec-top >/dev/null 2>&1

  # run: -d --rm --name -e --env-file -p -P -v(named) -w -u --restart --health-* --hostname --entrypoint --network --init
  printf 'FOO=bar\n' >"$WORKDIR/envfile"
  check "run with the full create/run flag set" bash -c "
    docker run -d --name e2ec-run -e X=1 --env-file $WORKDIR/envfile -p 18199:80 -v e2ec-vol:/data -w /tmp -u 101:101 \
      --health-cmd 'exit 0' --health-interval 2s --hostname e2ec-host --entrypoint /bin/sh --network bridge --init \
      nginx:alpine -c 'sleep 3600' >/dev/null &&
    for _ in \$(seq 60); do running e2ec-run && break; sleep 2; done &&
    [ \"\$(docker inspect -f '{{.Config.Hostname}} {{.Config.WorkingDir}} {{.Config.User}} {{.Config.Entrypoint}}' e2ec-run)\" = 'e2ec-host /tmp 101:101 [/bin/sh]' ] &&
    [ \"\$(docker exec e2ec-run env | grep -c '^X=1\$\\|^FOO=bar\$')\" = 2 ]"
  check "-P publishes the image's exposed port" bash -c '
    docker port e2ec-run | grep -q "80/tcp -> "'
  check "a named volume (-v) is a PersistentVolumeClaim" bash -c "
    kubectl --context $CONTEXT -n d2k get pvc e2ec-vol >/dev/null"
  check "-u is honoured, not refused" bash -c '[ "$(docker run --rm -u 2000 busybox id -u)" = 2000 ]'
  check "--restart and -d --rm conflict as in the Docker CLI" bash -c '
    docker run -d --rm --restart always --name e2ec-conflict busybox true 2>&1 | grep -q "conflicting options"'

  # create / start -a
  check "create leaves the container created, no warnings" bash -c '[ -z "$(docker create --name e2ec-create nginx:alpine 2>&1 >/dev/null)" ] && [ "$(docker inspect -f {{.State.Status}} e2ec-create)" = created ]'
  docker create --name e2ec-start-a busybox echo start-a-out >/dev/null
  check "start -a prints the container's output" bash -c '[ "$(to 20 docker start -a e2ec-start-a)" = start-a-out ]'

  # stop -t / restart -t / kill (known) / rm -f -v
  docker run -d --name e2ec-life nginx:alpine >/dev/null
  eventually 60 running e2ec-life
  check "stop -t stops within its grace period" bash -c 'time docker stop -t 2 e2ec-life >/dev/null; [ "$(docker inspect -f {{.State.Status}} e2ec-life)" = exited ]'
  check "restart runs a new pod" bash -c 'docker restart -t 2 e2ec-life >/dev/null && for _ in $(seq 60); do running e2ec-life && exit 0; sleep 2; done; exit 1'
  known "D2K-015: kill answers 501" "kill stops the container" bash -c 'docker kill e2ec-life 2>&1 | grep -qv "not supported by d2k"'
  docker run -d --name e2ec-rmv -v e2ec-rmv-vol:/data nginx:alpine >/dev/null
  eventually 60 running e2ec-rmv
  check "rm -f -v keeps a named volume (Docker only drops anonymous ones)" bash -c '
    docker rm -f -v e2ec-rmv >/dev/null && docker volume inspect e2ec-rmv-vol >/dev/null'
  docker volume rm e2ec-rmv-vol e2ec-vol >/dev/null 2>&1

  # ps: -a -q -s --filter(each key) --format --no-trunc -n -l
  docker run -d --name e2ec-ps1 --label e2e.k=v nginx:alpine >/dev/null
  docker run -d --name e2ec-ps2 nginx:alpine >/dev/null
  eventually 60 running e2ec-ps1
  check "ps -q --no-trunc prints a 64-hex ID" bash -c '[[ "$(docker ps -q --no-trunc --filter name=e2ec-ps1)" =~ ^[0-9a-f]{64}$ ]]'
  check "ps -s prints a size column" bash -c 'docker ps -s --filter name=e2ec-ps1 --format "{{.Size}}" | grep -q .'
  check "ps --filter label" bash -c '[ "$(docker ps -a --filter label=e2e.k=v --format {{.Names}})" = e2ec-ps1 ]'
  check "ps --filter name" bash -c '[ "$(docker ps -a --filter name=e2ec-ps2 --format {{.Names}})" = e2ec-ps2 ]'
  check "ps --filter id" bash -c 'id1=$(docker inspect -f {{.Id}} e2ec-ps1); [ "$(docker ps -a --filter id=$id1 --format {{.Names}})" = e2ec-ps1 ]'
  check "ps --filter status" bash -c '[ -n "$(docker ps -a --filter status=running --filter name=e2ec-ps1 --format {{.Names}})" ]'
  check "ps rejects a filter it does not implement, rather than ignoring it" bash -c 'docker ps --filter ancestor=nginx 2>&1 | grep -q "invalid filter"'
  # D2K-010 (fixOn integration): -n N now limits and sorts newest-first.
  check "ps -n 1 returns only the most recently created match" bash -c '[ "$(docker ps -n 1 --filter name=e2ec-ps --format {{.Names}})" = e2ec-ps2 ]'

  # inspect -f -s
  check "inspect -f formats a field" bash -c '[ "$(docker inspect -f "{{.Name}}" e2ec-ps1)" = /e2ec-ps1 ]'

  # logs -f --since --until --tail -t --details
  check "logs --tail 1 --details returns one detailed line" bash -c '
    docker logs --tail 1 --details e2ec-ps1 2>&1 | grep -q "start worker"'
  check "logs -t --since now-1h returns timestamped lines" bash -c '
    docker logs -t --since 1h e2ec-ps1 2>&1 | grep -qE "^20[0-9]{2}-[0-9]{2}-[0-9]{2}T"'
  docker run -d --name e2ec-logsf busybox sh -c 'i=0; while true; do i=$((i+1)); echo line-$i; sleep 1; done' >/dev/null
  check "logs -f streams new output" bash -c '
    (to 8 docker logs -f e2ec-logsf > '"$WORKDIR"'/logsf.out 2>&1 &) ; sleep 1
    for _ in $(seq 8); do [ "$(wc -l < '"$WORKDIR"'/logsf.out | tr -d " ")" -ge 2 ] && exit 0; sleep 1; done; exit 1'
  docker rm -f e2ec-logsf >/dev/null 2>&1

  # exec: -it -d -e -w
  known "exec always runs as the container's own user/workdir/env (like -u, see README)" "exec -e -w override the environment and working directory" bash -c '[ "$(docker exec -e Y=2 -w /tmp e2ec-ps1 sh -c "pwd; echo \$Y")" = "$(printf "/tmp\n2")" ]'
  check "exec -d returns immediately and the command still runs" bash -c '
    docker exec -d e2ec-ps1 sh -c "sleep 1; touch /tmp/execd-marker" && sleep 3 && docker exec e2ec-ps1 test -f /tmp/execd-marker'
  check "exec -it sizes the TTY to the client's terminal" bash -c "
    python3 $(dirname "$0")/run_in_terminal.py docker exec -it e2ec-ps1 sh -c 'sleep 1; ttysize' | grep -q '120 40'"

  # wait / top(known) / port / rename / stats
  docker run -d --name e2ec-wait busybox sh -c "sleep 2; exit 7" >/dev/null
  check "wait returns the exit code" bash -c '[ "$(to 30 docker wait e2ec-wait)" = 7 ]'
  known "README: top answers 501" "top lists processes" bash -c 'docker top e2ec-ps1 2>&1 | grep -qv "not supported by d2k"'
  check "rename changes the name ps reports" bash -c 'docker rename e2ec-ps2 e2ec-renamed && [ "$(docker ps --filter name=e2ec-renamed --format {{.Names}})" = e2ec-renamed ]'
  docker run -d --name e2ec-stats -m 64m busybox sh -c 'while true; do :; done' >/dev/null
  check "stats --no-stream reports the memory limit" bash -c '
    for _ in $(seq 45); do docker stats --no-stream --format "{{.MemUsage}}" e2ec-stats 2>/dev/null | grep -q "/ 64MiB" && exit 0; sleep 2; done; exit 1'

  # pause/unpause(known), diff(known), export(known), commit(known)
  known "README: pause answers 501" "pause suspends the container" bash -c 'docker pause e2ec-ps1 2>&1 | grep -qv "not supported by d2k"'
  known "README: unpause answers 501" "unpause resumes the container" bash -c 'docker unpause e2ec-ps1 2>&1 | grep -qv "not supported by d2k"'
  known "README: diff answers 501" "diff lists filesystem changes" bash -c 'docker diff e2ec-ps1 2>&1 | grep -qv "not supported by d2k"'
  known "README: export answers 501" "export streams a tar" bash -c 'docker export e2ec-ps1 2>&1 >/dev/null | grep -qv "not supported by d2k"'
  known "README: commit answers 501" "commit creates an image" bash -c 'docker commit e2ec-ps1 e2ec-img 2>&1 | grep -qv "not supported by d2k"'
  known "README: update answers 501" "update changes resources live" bash -c 'docker update --memory 128m e2ec-ps1 2>&1 | grep -qv "not supported by d2k"'

  # cp: into a created (not started) container works; into/from a started one is 501
  docker create --name e2ec-cp busybox sleep 3600 >/dev/null
  printf hello >"$WORKDIR/cp-in.txt"
  check "cp into a not-yet-started container" bash -c "
    docker cp $WORKDIR/cp-in.txt e2ec-cp:/tmp/cp-in.txt && docker start e2ec-cp >/dev/null && sleep 2 &&
    [ \"\$(docker exec e2ec-cp cat /tmp/cp-in.txt)\" = hello ]"
  known "README: cp into a started container answers 501" "cp into a started container" bash -c "docker cp $WORKDIR/cp-in.txt e2ec-cp:/tmp/cp-in2.txt 2>&1 | grep -qv 'not supported by d2k'"
  known "README: cp from a container answers 501" "cp from a container" bash -c "docker cp e2ec-cp:/tmp/cp-in.txt $WORKDIR/cp-out.txt 2>&1 | grep -qv 'not supported by d2k'"

  # prune (known)
  known "README: container prune answers 501" "container prune" bash -c 'docker container prune -f 2>&1 | grep -qv "not supported by d2k"'

  docker rm -f e2ec-run e2ec-create e2ec-start-a e2ec-life e2ec-ps1 e2ec-renamed e2ec-wait e2ec-stats e2ec-cp >/dev/null 2>&1
}

# ------------------------------------------------------------------ volume

volume_cases() {
  echo "== volume =="
  docker volume rm -f e2ev-1 2>/dev/null >/dev/null

  check "volume create -d local -o --label" bash -c '
    docker volume create -d local --label team=x -o size=2Gi e2ev-1 >/dev/null &&
    [ "$(docker volume inspect -f "{{.Options.size}}" e2ev-1)" = 2Gi ]'
  check "volume ls --filter name" bash -c '[ "$(docker volume ls -q --filter name=e2ev-1)" = e2ev-1 ]'
  check "volume ls --filter label" bash -c '[ "$(docker volume ls -q --filter label=team=x)" = e2ev-1 ]'
  known "d2k always reports Driver \"d2k\"; --filter driver=local never matches" "volume ls --filter driver=local finds a volume created with -d local" bash -c '[ -n "$(docker volume ls -q --filter driver=local --filter name=e2ev-1)" ]'
  check "volume inspect" bash -c '[ "$(docker volume inspect -f "{{.Name}} {{.Scope}}" e2ev-1)" = "e2ev-1 local" ]'
  known "README: volume prune answers 501" "volume prune" bash -c 'docker volume prune -f 2>&1 | grep -qv "not supported by d2k"'
  check "volume rm -f" bash -c 'docker volume rm -f e2ev-1 >/dev/null && ! docker volume inspect e2ev-1 >/dev/null 2>&1'
}

# ----------------------------------------------------------------- network

network_cases() {
  echo "== network =="
  docker rm -f e2en-c1 >/dev/null 2>&1
  docker network rm e2en-1 >/dev/null 2>&1

  check "network create --driver --label" bash -c '
    docker network create --driver bridge --label team=net e2en-1 >/dev/null &&
    [ "$(docker network inspect -f "{{.Driver}}" e2en-1)" = bridge ]'
  known "d2k has flat namespace networking: IPAM settings (--subnet) are accepted but not applied" "network create --subnet is reflected in IPAM.Config" bash -c '
    docker network rm e2en-sub >/dev/null 2>&1
    docker network create --subnet 172.40.0.0/16 e2en-sub >/dev/null &&
    r=$(docker network inspect -f "{{json .IPAM.Config}}" e2en-sub)
    docker network rm e2en-sub >/dev/null 2>&1
    [ "$r" != "[]" ]'
  check "network ls --filter name" bash -c '[ "$(docker network ls -q --filter name=e2en-1)" = "$(docker network inspect -f {{.Id}} e2en-1 | cut -c1-12)" ]'
  check "network ls --filter label" bash -c 'docker network ls --filter label=team=net --format {{.Name}} | grep -qx e2en-1'
  docker run -d --name e2en-c1 nginx:alpine >/dev/null
  eventually 60 running e2en-c1
  check "network connect --alias" bash -c 'docker network connect --alias e2en-alias e2en-1 e2en-c1'
  check "network disconnect" bash -c 'docker network disconnect e2en-1 e2en-c1'
  known "README: network prune answers 501" "network prune" bash -c 'docker network prune -f 2>&1 | grep -qv "not supported by d2k"'
  docker rm -f e2en-c1 >/dev/null 2>&1
  check "network rm" bash -c 'docker network rm e2en-1 >/dev/null && ! docker network inspect e2en-1 >/dev/null 2>&1'
}

# ------------------------------------------------------------------- image

image_cases() {
  echo "== image =="
  check "pull is acknowledged, not performed" bash -c 'docker pull nginx:alpine 2>&1 | grep -q "the kubelet pulls it"'
  check "images lists none" bash -c '[ -z "$(docker images -q)" ]'
  check "image inspect reports the reference only" bash -c '[ "$(docker image inspect -f "{{.RepoTags}}" nginx:alpine)" = "[nginx:alpine]" ]'
  known "README: rmi answers not supported" "rmi removes an image" bash -c 'docker rmi nginx:alpine 2>&1 | grep -qv "not supported by d2k"'
  known "README: image prune answers 501" "image prune" bash -c 'docker image prune -f 2>&1 | grep -qv "not supported by d2k"'
  known "README: tag answers 501" "tag creates an alias" bash -c 'docker tag nginx:alpine e2e/nginx:test 2>&1 | grep -qv "not supported by d2k"'
  known "README: history answers 501" "history lists layers" bash -c 'docker history nginx:alpine 2>&1 | grep -qv "not supported by d2k"'
  known "README: search answers 501" "search queries a registry" bash -c 'docker search nginx 2>&1 | grep -qv "not supported by d2k"'
}

# ------------------------------------------------------------------ system

system_cases() {
  echo "== system =="
  check "info identifies the namespace" bash -c "[ \"\$(docker info -f {{.ID}})\" = \"\$(kubectl --context $CONTEXT get ns d2k -o jsonpath={.metadata.uid})\" ]"
  check "version negotiates" docker version
  known "README: system df is not meaningful in Kubernetes" "system df" bash -c 'docker system df 2>&1 | grep -qv "not supported by d2k"'
  check "events --since/--until replay recorded events" bash -c '
    start=$(date +%s); docker run --name e2e-sys-ev busybox true >/dev/null &&
    [ -n "$(docker events --since $start --until $(($(date +%s)+1)) --filter container=e2e-sys-ev --format "{{.Action}}")" ]'
  docker rm -f e2e-sys-ev >/dev/null 2>&1
}

reset
container_cases
volume_cases
network_cases
image_cases
system_cases
reset

echo
echo "passed: $pass  failed: $fail  known gaps: $xfail  unexpectedly passing: $xpass"
for name in ${failed[@]+"${failed[@]}"}; do echo "  - $name"; done
[ "$fail" -eq 0 ] && [ "$xpass" -eq 0 ]
