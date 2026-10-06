# Corpus: real-world compose apps on d2k

Thirteen compose projects a real user actually runs, each kept at
`test/e2e/corpus/<app>/compose.yaml` with its public source noted at the top
of the file (two, `secrets-configs` and `migrate-once`, are dedicated
fixtures for compose features too specific to land naturally in a public
app's compose file). Every app uses `image:` only, at a pinned tag: no
`build:`.

| app | what it checks |
|---|---|
| `wordpress-mysql` | the classic LAMP-style two-service app |
| `gitea-postgres` | healthcheck + `depends_on: service_healthy`, a Git server with real repo data |
| `nextcloud` | three services (app, db, cache), file storage in a named volume |
| `prometheus-grafana` | two independent apps, a config from `content` |
| `traefik-whoami` | a reverse proxy with its routes from a config (no Docker provider: there is no socket) |
| `paperless-ngx` | a real Django app, stands in for "Django/Flask+Postgres+Redis" |
| `n8n-postgres` | a workflow engine, JSON state in a named volume |
| `uptime-kuma` | single-service app with its own SQLite file in a volume |
| `minio` | an init/migration one-shot service (`mc` creates a bucket, then exits) |
| `ghost-mysql` | a Node.js app that needs no build image |
| `secrets-configs` | a secret from `environment`, a config from `content` |
| `migrate-once` | `depends_on: service_completed_successfully` gating a dependent service |
| `metabase-postgres` | another real BI app, Postgres-backed |

## Running it

Requires the same tools as `test/e2e/run.sh` (`docker`, `kubectl`, `python3`)
and a d2k standalone instance reachable the way `env/README.md` describes.
Defaults match the shared environment; override `CONTEXT`, `STANDALONE` and
`PKI` to target another one (soak, say), the same variables `run.sh` and
`compose_spec.py` take:

```sh
test/e2e/corpus/run.py                  # every app, in turn
test/e2e/corpus/run.py n8n-postgres minio   # only the named apps
```

Each app, in turn:

1. `compose up -d`, then waits for its HTTP check(s) to answer as a real
   client would (the login page, a health or version API, a readiness
   endpoint) — not just for the container to start.
2. `compose ps`, `compose logs`, `docker exec` into the main service.
3. `compose down` (keeps volumes), `compose up -d` again, and re-checks the
   same HTTP endpoints: this is only a PASS if the app's data (its database,
   or its on-disk state) survived.
4. Flips a label on the main service and runs `compose up -d` again, which
   d2k must see as a config change and recreate the container for; checks
   the app still answers afterwards.
5. `compose down -v`, then polls `kubectl` until no Deployment, Pod, Service,
   PVC or Secret carries that project's `com.docker.compose.project` label.

Exits non-zero if any app's checks failed, and prints one `PASS`/`FAIL` line
per app at the end.

## Ports

Each app publishes on a fixed `181NN` host port (18180-18194) to avoid
clashing with other e2e fixtures (which use 18080s) and with each other when
two lanes run the corpus on different clusters at once.

## What this does not check

Login flows that need a real browser session (Nextcloud's admin UI,
Grafana's dashboard editor) are checked through their HTTP APIs instead
(`/status.php`, `/api/health`, `/api/v1/version`, ...), which is how an
operator or Portainer would check them too. `uptime-kuma` and `traefik`'s own
dashboard are checked by content match on the root page rather than a login,
since neither exposes a simple read-only API for this.
