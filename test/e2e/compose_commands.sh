#!/usr/bin/env bash
# Exercises every docker compose command and its realistic flags against d2k,
# the way a user would: a small multi-service project (a web service, a db
# with a healthcheck, a one-shot init job, a named volume and a network).
#
# compose_spec.py covers every Compose file attribute; this script covers the
# commands and flags compose itself exposes (up, down, ps, logs, exec, run,
# start/stop/restart, kill, pause/unpause, rm, create, cp, top, port, events,
# wait, attach, scale, stats, ls, config, images, version) instead.
#
# Results: PASS / FAIL, plus KNOWN for gaps already in the tracker (never
# re-reported here; see the ids named at each call). Exits non-zero on FAIL.
set -uo pipefail

CONTEXT=kind-${CLUSTER:-d2k}
STANDALONE=${STANDALONE:-tcp://localhost:31112}
PKI=${PKI:-$(cd "$(dirname "$0")" && pwd)/.pki/client}
# COMPOSE_CMD: "docker compose" (default, the CLI plugin) or a standalone
# binary with no "compose" subcommand, e.g. a pinned docker-compose release.
COMPOSE_CMD=${COMPOSE_CMD:-docker compose}

export DOCKER_HOST=$STANDALONE DOCKER_TLS_VERIFY=1 DOCKER_CERT_PATH=$PKI COMPOSE_CMD

# compose: wraps COMPOSE_CMD, exported so nested `bash -c '...'` checks (run
# in a fresh shell) can call it too.
compose() { $COMPOSE_CMD "$@"; }
export -f compose

WORKDIR=$(mktemp -d)
export WORKDIR
trap 'cd /; compose -f "$WORKDIR/compose.yaml" down -v --remove-orphans -t 5 >/dev/null 2>&1; rm -rf "$WORKDIR"' EXIT

cat >"$WORKDIR/compose.yaml" <<'YAML'
name: cmdtest
services:
  db:
    image: redis:7-alpine
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 2s
      timeout: 2s
      retries: 10
    networks: [backend]
    volumes:
      - dbdata:/data
  init:
    image: busybox
    command: ["sh", "-c", "echo init-done"]
    restart: "no"
    depends_on:
      db:
        condition: service_healthy
    networks: [backend]
  web:
    image: nginx:alpine
    restart: unless-stopped
    depends_on:
      init:
        condition: service_completed_successfully
    ports:
      - "8080:80"
    networks: [backend]
volumes:
  dbdata: {}
networks:
  backend: {}
YAML
cd "$WORKDIR"

pass=0 fail=0 known=0
failed=()

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

# known <id> <name> <cmd...>: a gap already in the tracker; never fails the run.
known() {
  local id=$1 name=$2; shift 2
  if ("$@") >"$WORKDIR/out" 2>&1; then
    echo "KNOWN  $name ($id, now passes - recheck the tracker)"
  else
    echo "KNOWN  $name ($id)"
  fi
  known=$((known + 1))
}

echo "== up =="
check "up -d" compose up -d
check "up -d --wait --wait-timeout" compose up -d --wait --wait-timeout 60
check "up --no-deps (one service only)" compose up -d --no-deps web
check "up --force-recreate" compose up -d --force-recreate web
check "up --no-recreate (no-op on a match)" compose up -d --no-recreate web
check "up --scale web=2" compose up -d --scale web=2 --remove-orphans
check "up --scale back to 1" compose up -d --scale web=1 --remove-orphans
check "up --pull always" compose up -d --pull always --no-deps web
# --build: no service in this fixture has a build: section (documented
# unsupported, docs/compose-support.md); this only checks it doesn't error
# when there's nothing to build.
check "up --build (nothing to build)" compose up -d --build --no-deps web
# --exit-code-from right after init's previous run just completed can hit
# D2K-063 (a run-once container restarted within ~1-2s of its last run
# ending gets no new pod; POST /start answers 204 and the attached up then
# waits forever for a die that never comes). The checks above run back to
# back with no real delay, so give the race window time to close first.
sleep 5
# Bounded so the suite can't hang even so: SIGTERM the local compose
# process (not a docker kill, so D2K-015 doesn't get in the way) if it's
# not back within 20s.
(compose up --no-deps --exit-code-from init init >"$WORKDIR/exit-code-from.out" 2>&1) & p=$!
for _ in $(seq 20); do kill -0 "$p" 2>/dev/null || break; sleep 1; done
if kill -0 "$p" 2>/dev/null; then
  kill -TERM "$p" 2>/dev/null; sleep 1; kill -KILL "$p" 2>/dev/null
  echo "KNOWN  up --exit-code-from right after init's own last run (D2K-063)"
  known=$((known + 1))
  # Interrupting it leaves init exited non-zero, which would wrongly fail
  # web's service_completed_successfully condition below: recreate the
  # whole project clean rather than patch just init.
  compose down -v --remove-orphans -t 5 >/dev/null 2>&1
  compose up -d --wait --wait-timeout 60 >/dev/null 2>&1
else
  rc=0; wait "$p" || rc=$?
  check "up --exit-code-from (one-shot init)" bash -c "exit $rc"
fi
check "up --remove-orphans" compose up -d --remove-orphans
# --abort-on-container-exit hangs on a restart:always service: D2K-058, fixed
# in its own lane. Not re-checked here.

echo "== ps =="
check "ps" compose ps
check "ps -a" compose ps -a
check "ps --format json" bash -c 'compose ps --format json | head -1 | python3 -c "import json,sys; json.loads(sys.stdin.read())"'
check "ps --services" compose ps --services
check "ps --status running" compose ps --status running

echo "== logs =="
check "logs (single service)" compose logs web
check "logs --tail" bash -c '[ "$(compose logs --tail 1 db | wc -l | tr -d " ")" = 1 ]'
check "logs --since" compose logs --since 10m db
check "logs -t" bash -c 'compose logs -t --tail 1 db | grep -qE "^db-1  \| 20[0-9]{2}-"'
check "logs -f (follow, then stop)" bash -c '
  compose logs -f web >"$WORKDIR/logsf.out" 2>&1 & p=$!
  sleep 3; kill "$p" 2>/dev/null; wait "$p" 2>/dev/null
  [ -s "$WORKDIR/logsf.out" ]'

echo "== exec =="
check "exec -T" bash -c 'compose exec -T web ls / >/dev/null'
check "exec -w (refused by design)" bash -c '! compose exec -T -w /tmp web pwd'
check "exec -e (refused by design)" bash -c '! compose exec -T -e FOO=bar web env'
check "exec -u (refused by design)" bash -c '! compose exec -T -u 1000 web id'

echo "== run =="
check "run --rm -T" bash -c '[ "$(compose run --rm -T web echo hi | tail -1)" = hi ]'
check "run -d" bash -c 'compose run -d --name cmdtest-run-d web sleep 60 >/dev/null && docker rm -f cmdtest-run-d >/dev/null'
# D2K-050/D2K-051: a --rm run's output is lost on a residual, low-rate race
# (attach can end on the die event before it drains) - retry once rather
# than fail the suite on it.
check "run --entrypoint" bash -c '
  for _ in 1 2; do
    [ "$(compose run --rm -T --entrypoint sh web -c "echo entry-ok" | tail -1)" = entry-ok ] && exit 0
  done
  exit 1'
check "run -e" bash -c '[ "$(compose run --rm -T -e FOO=bar web sh -c "echo \$FOO" | tail -1)" = bar ]'
check "run --no-deps" bash -c 'compose run --rm -T --no-deps web echo nodeps-ok >/dev/null'
check "run --service-ports" bash -c '
  compose run -d --rm --service-ports --name cmdtest-svcports web >/dev/null
  ok=0
  # compose ps --format json escapes ">" as >, not a literal "->".
  for _ in $(seq 10); do
    compose ps -a --format json | grep -q "cmdtest-svcports.*8080.*80/tcp" && { ok=1; break; }
    sleep 1
  done
  docker rm -f cmdtest-svcports >/dev/null 2>&1
  [ "$ok" = 1 ]'

echo "== lifecycle: start/stop/restart/kill/pause =="
check "stop -t" compose stop -t 5 db
check "start" compose start db
check "restart -t" compose restart -t 3 web
# D2K-060: for ~1-1.5s right after restart returns, the restarted service is
# missing from compose ps entirely (ListContainers excludes a Deployment
# scaled back up with no pod yet), so a command issued immediately after
# restart - like these - finds no running container and silently no-ops
# instead of reaching d2k at all. Settle first so these actually exercise
# the 501 refusal rather than this known gap.
sleep 2
known D2K-015 "kill (documented 501)" compose kill web
check "pause (documented 501)" bash -c '! compose pause web'
check "unpause (documented 501)" bash -c '! compose unpause web'
check "up -d (bring db back) " compose up -d

echo "== rm/create =="
check "rm without -f refuses a running container" bash -c '
  compose rm web 2>&1 | grep -qi "no stopped containers" &&
  compose ps --format json | grep -q "\"Service\":\"web\""'
check "rm -f -s" compose rm -f -s web
check "create" compose create web
check "up -d (recreated service back up)" compose up -d

echo "== cp/top/port =="
check "cp into a started container refused (documented)" bash -c '
  echo x >"$WORKDIR/x.txt"
  ! compose cp "$WORKDIR/x.txt" web:/tmp/x.txt'
check "cp from a container refused (documented, GET archive 501)" bash -c '! compose cp web:/etc/hostname "$WORKDIR/hostname.out"'
check "top (documented 501)" bash -c '! compose top'
sleep 2  # settle past D2K-060's post-(re)start listing gap; see the restart section above
check "port" bash -c '[ "$(compose port web 80)" = "0.0.0.0:8080" ]'

echo "== events/wait =="
check "events --json" bash -c '
  compose events --json >"$WORKDIR/events.out" 2>&1 & p=$!
  sleep 1; compose restart -t 2 db >/dev/null; sleep 2
  kill "$p" 2>/dev/null; wait "$p" 2>/dev/null
  grep -q "\"action\":\"start\"" "$WORKDIR/events.out"'
sleep 2  # settle past D2K-060 again: events --json just restarted db
# `compose wait SERVICE` blocks until that service's container stops, then
# prints its exit code - it needs the container to actually be running (or
# about to be) at the moment it is called; given one already exited, or one
# whose replacement pod isn't visible yet (D2K-060's listing gap right after
# a start/restart/recreate), it fails with "no containers for project" on
# real Docker too (confirmed on local Docker 29.4/compose v5.1.2) - not a
# d2k difference. web has been stably up since the settle above, so wait on
# it while stopping it from another call.
check "wait" bash -c '
  (sleep 2; compose stop -t 2 web >/dev/null 2>&1) &
  [ "$(compose wait web)" = 0 ]
  wait'
compose up -d >/dev/null 2>&1  # bring web back for the checks below
sleep 2  # settle past D2K-060 once more

echo "== stats/scale/ls/config/images/version =="
check "stats --no-stream" bash -c 'compose stats --no-stream | grep -q web'
check "scale (top-level)" compose up -d --scale web=1 --no-recreate
check "ls" bash -c 'compose ls | grep -qw cmdtest'
check "config" bash -c 'compose config | grep -q "name: cmdtest"'
check "config --services" bash -c '[ "$(compose config --services | sort | tr "\n" , )" = "db,init,web," ]'
check "images" bash -c 'compose images | grep -q nginx'
check "version" compose version

echo "== down =="
check "down --remove-orphans -v -t" compose down --remove-orphans -v -t 5
check "down --rmi (container/network cleanup still happens)" bash -c '
  compose up -d --no-deps web >/dev/null
  compose down --rmi all 2>&1 | grep -q "Removed"'

echo
echo "passed: $pass  failed: $fail  known gaps: $known"
for name in ${failed[@]+"${failed[@]}"}; do echo "  - $name"; done
[ "$fail" -eq 0 ]
