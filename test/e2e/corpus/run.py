#!/usr/bin/env python3
"""Real-world compose apps run end to end against d2k's standalone instance.

Each app in CORPUS is a published compose project (image: only, pinned tags)
kept at test/e2e/corpus/<app>/compose.yaml, with its source noted in the
file. For each app this script drives the lifecycle a real user would:

    up -d, wait, check it works (HTTP, or a write and read back)
    logs / ps / exec
    down, up again, check the data is still there
    up -d after a label change, check the container was recreated and still works
    down -v, check every object d2k created is gone

Usage:
    test/e2e/corpus/run.py            every app
    test/e2e/corpus/run.py n8n minio  only the named apps (directory names)
"""
import http.cookiejar
import json
import os
import shutil
import ssl
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
E2E = os.path.dirname(HERE)
CONTEXT = os.environ.get("CONTEXT", "kind-d2k")
STANDALONE = os.environ.get("STANDALONE", "tcp://localhost:31112")
PKI = os.environ.get("PKI", os.path.join(E2E, ".pki", "client"))
NS = "d2k"
ENV = dict(os.environ, DOCKER_HOST=STANDALONE, DOCKER_TLS_VERIFY="1", DOCKER_CERT_PATH=PKI)


def sh(*args, timeout=60, env=ENV, cwd=None, check=False):
    return subprocess.run(args, cwd=cwd, env=env, capture_output=True, text=True, timeout=timeout)


def compose(cwd, project, *args, timeout=240, env=ENV):
    return sh("docker", "compose", "-p", project, *args, cwd=cwd, timeout=timeout, env=env)


def kubectl(*args, timeout=60):
    return sh("kubectl", "--context", CONTEXT, *args, env=os.environ.copy(), timeout=timeout)


def eventually(timeout, fn, interval=3):
    """Retry fn() (returns truthy/falsy) until it succeeds or timeout elapses."""
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            if fn():
                return True
        except Exception as e:
            last = e
        time.sleep(interval)
    if last:
        print(f"         last error: {last}")
    return False


def http_get(port, path, timeout=5):
    req = urllib.request.Request(f"http://localhost:{port}{path}", headers={"User-Agent": "corpus"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.status, r.read().decode("utf-8", "replace")


def http_ok(port, path, want, timeout=5):
    try:
        status, body = http_get(port, path, timeout)
        return status < 500 and want in body
    except (urllib.error.URLError, ConnectionError, TimeoutError, OSError):
        return False


def http_with_jar(port, path, cookiejar, fields=None, timeout=10):
    """GET (fields=None) or POST, through a cookiejar an earlier call's Set-Cookie
    feeds into the next call's request -- a real browser's session, not one isolated
    request each time."""
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cookiejar))
    data = urllib.parse.urlencode(fields).encode() if fields is not None else None
    req = urllib.request.Request(f"http://localhost:{port}{path}", data=data, headers={"User-Agent": "corpus"})
    with opener.open(req, timeout=timeout) as r:
        return r.status, r.geturl(), r.read().decode("utf-8", "replace")


class PortForward:
    """A published port becomes a LoadBalancer Service, which a bare kind cluster never
    assigns an external IP to (no cloud load balancer), so the app's host:// ports are not
    reachable from the test runner the way they would be on real Docker. `kubectl
    port-forward` to the pod reaches the app the same way a real operator would from outside
    the cluster, and does not depend on kind's node-to-host port mapping.

    `compose up -d` returns as soon as the container is created, well before its pod is
    Running (docker compose does not wait for a health check unless the service declares
    one). `kubectl port-forward deploy/<x>` run right after that exits immediately with
    "pod is not running. Current status=Pending" instead of waiting, so a single attempt
    started too early fails for good. Retry starting it until the deadline.

    A compose "Recreate" (e.g. after a label change) makes a Deployment/Service that keep
    whatever throwaway name docker compose's create-then-rename dance first gave them --
    `docker rename` only patches an annotation (internal/adapter/container.go
    RenameContainer), it never renames the Deployment -- so `<project>-<service>-1` can
    stop existing even though `docker ps` still shows the container by that name. Resolve
    the target by the compose labels a rename never touches instead of assuming the name."""

    def __init__(self, project, service, local_port, remote_port, ready_timeout=90):
        self.project, self.service = project, service
        self.local_port, self.remote_port = local_port, remote_port
        self.ready_timeout = ready_timeout
        self.proc = None

    def _resolve(self):
        out = kubectl("-n", NS, "get", "deploy", "-l",
                      f"com.docker.compose.project={self.project},com.docker.compose.service={self.service}",
                      "-o", "name")
        names = [l for l in out.stdout.splitlines() if l.strip()]
        return names[0] if names else f"deploy/{self.project}-{self.service}-1"

    def _spawn(self):
        target = self._resolve()
        self.proc = subprocess.Popen(
            ["kubectl", "--context", CONTEXT, "-n", NS, "port-forward", target,
             f"{self.local_port}:{self.remote_port}"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    def __enter__(self):
        deadline = time.time() + self.ready_timeout
        while True:
            self._spawn()
            while self.proc.poll() is None and not self._listening() and time.time() < deadline:
                time.sleep(0.5)
            if self.proc.poll() is None or time.time() >= deadline:
                return self  # listening, or out of time either way
            time.sleep(1)  # the forward exited (pod not Running yet) -- retry it

    def ensure_up(self):
        """kubectl port-forward exits -- not just logs and keeps going -- the moment a
        proxied connection fails while the app inside is still starting up ("lost
        connection to pod"), so the forward can die well before the app, and well before
        a caller polling it for up to an app's whole startup budget would expect. Callers
        doing their own polling loop call this each iteration to respawn it."""
        if self.proc.poll() is not None:
            self._spawn()

    def _listening(self):
        try:
            with urllib.request.urlopen(f"http://localhost:{self.local_port}/", timeout=1):
                pass
            return True
        except urllib.error.HTTPError:
            return True  # any HTTP response means the forward is up
        except (urllib.error.URLError, ConnectionError, TimeoutError, OSError):
            return False

    def __exit__(self, *exc):
        if self.proc:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.proc.kill()


class Result:
    def __init__(self, app):
        self.app = app
        self.steps = []  # (name, ok, detail)

    def step(self, name, ok, detail=""):
        self.steps.append((name, ok, detail))
        mark = "PASS" if ok else "FAIL"
        print(f"  {mark}  {name}" + (f": {detail}" if detail and not ok else ""))
        return ok

    @property
    def failed(self):
        return any(not ok for _, ok, _ in self.steps)


# ---------------------------------------------------------------- apps

def default_recreate(app, r):
    """Flip the corpus.mark label on the main service and confirm it is still reachable."""
    if not app.get("main_service") or not app.get("http"):
        return
    with open(os.path.join(app["dir"], "compose.yaml")) as f:
        original = f.read()
    changed = original.replace('corpus.mark: "v1"', 'corpus.mark: "v2"', 1)
    if changed == original:
        return
    path = os.path.join(app["dir"], "compose.yaml")
    try:
        with open(path, "w") as f:
            f.write(changed)
        up = compose(app["dir"], app["project"], "up", "-d", env=dict(ENV, **app.get("env", {})))
        r.step("compose up after a label change recreates the service", up.returncode == 0, up.stderr)
        check_http(r, app["project"], app["http"][:1], 90, "after the recreate")
    finally:
        with open(path, "w") as f:
            f.write(original)


# Each http entry is (service, local_port, remote_port, path, want): local_port is only
# the kubectl port-forward's local end (see PortForward); it need not match the compose
# file's published port, since a bare kind cluster never routes a LoadBalancer Service's
# port to the host (no cloud load balancer assigns it an external IP).
CORPUS = [
    dict(name="wordpress-mysql", dir=os.path.join(HERE, "wordpress-mysql"), project="corpus-wp",
         main_service="wordpress", services=["wordpress", "db"],
         http=[("wordpress", 18180, 80, "/wp-login.php", "user_login")], up_timeout=240),
    dict(name="gitea-postgres", dir=os.path.join(HERE, "gitea-postgres"), project="corpus-gitea",
         main_service="gitea", services=["gitea", "db"],
         http=[("gitea", 18181, 3000, "/api/v1/version", "version")], up_timeout=180),
    dict(name="nextcloud", dir=os.path.join(HERE, "nextcloud"), project="corpus-nc",
         main_service="nextcloud", services=["nextcloud", "db", "redis"],
         http=[("nextcloud", 18182, 80, "/status.php", '"installed":true')], up_timeout=300),
    dict(name="prometheus-grafana", dir=os.path.join(HERE, "prometheus-grafana"), project="corpus-promgraf",
         main_service="grafana", services=["prometheus", "grafana"],
         http=[("prometheus", 18183, 9090, "/-/ready", "Ready"),
               ("grafana", 18184, 3000, "/api/health", '"database": "ok"')], up_timeout=120),
    dict(name="traefik-whoami", dir=os.path.join(HERE, "traefik-whoami"), project="corpus-traefik",
         main_service="whoami", services=["traefik", "whoami"],
         http=[("traefik", 18185, 80, "/", "Hostname:")], up_timeout=60,
         # traefik/whoami:v1.10 is a scratch image with no shell or coreutils; exec into
         # traefik (alpine-based) instead, same as a real operator would have to.
         exec_service="traefik"),
    dict(name="paperless-ngx", dir=os.path.join(HERE, "paperless-ngx"), project="corpus-paperless",
         main_service="webserver", services=["webserver", "db", "broker"],
         http=[("webserver", 18187, 8000, "/api/", "correspondents")], up_timeout=300),
    dict(name="n8n-postgres", dir=os.path.join(HERE, "n8n-postgres"), project="corpus-n8n",
         main_service="n8n", services=["n8n", "db"],
         http=[("n8n", 18188, 5678, "/healthz", '"status":"ok"')], up_timeout=180),
    dict(name="uptime-kuma", dir=os.path.join(HERE, "uptime-kuma"), project="corpus-kuma",
         main_service="uptime-kuma", services=["uptime-kuma"],
         http=[("uptime-kuma", 18189, 3001, "/", "Uptime Kuma")], up_timeout=120),
    dict(name="minio", dir=os.path.join(HERE, "minio"), project="corpus-minio",
         main_service="minio", services=["minio", "init"],
         http=[("minio", 18190, 9000, "/minio/health/live", "")], up_timeout=120),
    dict(name="ghost-mysql", dir=os.path.join(HERE, "ghost-mysql"), project="corpus-ghost",
         main_service="ghost", services=["ghost", "db"],
         http=[("ghost", 18192, 2368, "/", "Ghost")], up_timeout=180),
    dict(name="secrets-configs", dir=os.path.join(HERE, "secrets-configs"), project="corpus-secrets",
         main_service="app", services=["app"],
         http=[("app", 18193, 80, "/health", "ok")], up_timeout=60,
         env={"API_TOKEN": "s3cret-token"}, secret_check=("app", 18193, 80, "/token", "s3cret-token")),
    dict(name="migrate-once", dir=os.path.join(HERE, "migrate-once"), project="corpus-migrate",
         main_service="app", services=["migrate", "app"], http=[], up_timeout=60),
    dict(name="metabase-postgres", dir=os.path.join(HERE, "metabase-postgres"), project="corpus-metabase",
         main_service="metabase", services=["metabase", "db"],
         http=[("metabase", 18194, 3000, "/api/health", '"status":"ok"')], up_timeout=240),
]


def check_http(r, project, app_http, timeout, label):
    ok = True
    for service, local_port, remote_port, path, want in app_http:
        with PortForward(project, service, local_port, remote_port) as pf:
            def probe(p=local_port, h=path, w=want, pf=pf):
                pf.ensure_up()
                return http_ok(p, h, w)
            this_ok = eventually(timeout, probe)
        ok = r.step(f"{label}: {path} answers as expected", this_ok) and ok
    return ok


WP_ADMIN = dict(user="admin", password="CorpusPass123!", email="admin@example.com")


def provision_wordpress(r, project, http_entry, timeout):
    """A fresh wordpress image is never installed: wp-login.php redirects to the
    install wizard until someone completes it, exactly like a real user's first visit.
    Drive that wizard the way a browser would, so later checks see a working site
    (and can log in), not a perpetual installer redirect."""
    service, local_port, remote_port, _, _ = http_entry
    jar = http.cookiejar.CookieJar()

    def install_and_login(pf):
        pf.ensure_up()
        http_with_jar(local_port, "/wp-admin/install.php?step=1", jar, {"language": "en_US"})
        _, _, body = http_with_jar(local_port, "/wp-admin/install.php?step=2", jar, {
            "weblog_title": "Corpus Test", "user_name": WP_ADMIN["user"],
            "admin_password": WP_ADMIN["password"], "admin_password2": WP_ADMIN["password"],
            "pw_weak": "1", "admin_email": WP_ADMIN["email"],
            "Submit": "Install WordPress", "language": "",
        })
        if "Success!" not in body:
            return False
        # wp-login.php refuses to log in without the wordpress_test_cookie a plain GET
        # of the login page sets first -- a real browser always has it by the time it
        # submits the form.
        http_with_jar(local_port, "/wp-login.php", jar)
        _, url, _ = http_with_jar(local_port, "/wp-login.php", jar, {
            "log": WP_ADMIN["user"], "pwd": WP_ADMIN["password"], "wp-submit": "Log In",
            "redirect_to": "/wp-admin/", "testcookie": "1",
        })
        return "wp-admin" in url

    with PortForward(project, service, local_port, remote_port) as pf:
        ok = eventually(timeout, lambda: install_and_login(pf))
    r.step("WordPress install wizard completes and the admin can log in", ok)


def project_objects(kind, project):
    """Objects d2k made for this project, for this one kind. Only Deployments and PVCs
    carry `com.docker.compose.project`; Pods carry `d2k.portainer.io/compose-project`
    instead, and per-container Services carry neither -- and a container recreated via
    docker's create-then-rename dance keeps the Deployment/Service names it was first
    created under forever (docker rename only patches an annotation, see
    internal/adapter/container.go RenameContainer), which for a compose "Recreate" is a
    throwaway hash, not `<project>-<service>-N`. `d2k.portainer.io/managed-by=d2k` (the
    label env/reset.sh itself cleans up by) is the one label every kind carries; match
    either the project's usual naming or its compose labels, which a rename never touches."""
    out = kubectl("-n", NS, "get", kind, "-l", "d2k.portainer.io/managed-by=d2k", "-o", "json")
    try:
        items = json.loads(out.stdout).get("items", [])
    except ValueError:
        return []
    found = []
    for it in items:
        meta = it.get("metadata", {})
        name = meta.get("name", "")
        labels = meta.get("labels", {}) or {}
        if (labels.get("com.docker.compose.project") == project
                or name.startswith(f"{project}-") or name == f"compose-{project}"):
            found.append(name)
    return found


def run_app(app):
    r = Result(app["name"])
    print(f"== {app['name']} ({app['project']})")
    dir_, project = app["dir"], app["project"]
    try:
        run_app_body(app, r, dir_, project)
    except subprocess.TimeoutExpired as e:
        r.step(f"timed out: {e.cmd}", False, str(e))
    except Exception as e:
        r.step(f"crashed: {type(e).__name__}", False, str(e))
    finally:
        cleanup = sh("docker", "compose", "-p", project, "down", "-v", "-t", "5",
                     cwd=dir_, env=dict(ENV, **app.get("env", {})), timeout=90)
        if cleanup.returncode != 0:
            print(f"         cleanup down -v failed: {cleanup.stderr.strip()[-400:]}")
    return r


def run_app_body(app, r, dir_, project):
    env = dict(ENV, **app.get("env", {}))

    up = sh("docker", "compose", "-p", project, "up", "-d", cwd=dir_, env=env, timeout=app["up_timeout"] + 30)
    if not r.step("compose up", up.returncode == 0, (up.stdout + up.stderr)[-800:]):
        return

    if app["name"] == "wordpress-mysql":
        provision_wordpress(r, project, app["http"][0], app["up_timeout"])

    check_http(r, project, app["http"], app["up_timeout"], "HTTP")

    if app["name"] == "minio":
        r.step("mc init job created the bucket",
               eventually(60, lambda: "init-done" in compose(dir_, project, "logs", "init").stdout))

    if app["name"] == "migrate-once":
        r.step("the app only started after the migration completed",
               eventually(60, lambda: "done" in sh("docker", "exec", f"{project}-app-1", "cat", "/shared/migrated").stdout))

    if "secret_check" in app:
        service, local_port, remote_port, path, want = app["secret_check"]
        with PortForward(project, service, local_port, remote_port) as pf:
            def probe(p=local_port, h=path, w=want, pf=pf):
                pf.ensure_up()
                return http_ok(p, h, w)
            r.step("the secret from `environment` reached the container", eventually(30, probe))

    # -a: a one-shot init/migration service (restart: "no", already exited 0) is exactly
    # as real `docker compose ps` without it would drop it, not a d2k thing to check for.
    ps = compose(dir_, project, "ps", "-a", "--format", "json")
    n_up = len([l for l in ps.stdout.splitlines() if l.strip()])
    r.step("compose ps lists the project's containers", n_up == len(app["services"]), ps.stdout)

    logs = compose(dir_, project, "logs", app["main_service"])
    r.step("compose logs returns output", bool(logs.stdout.strip()) or bool(logs.stderr.strip()))

    exec_ctr = f"{project}-{app.get('exec_service', app['main_service'])}-1"
    ex = sh("docker", "exec", exec_ctr, "sh", "-c", "echo corpus-exec-ok")
    r.step("exec runs in the main service's container", ex.returncode == 0 and "corpus-exec-ok" in ex.stdout)

    down = sh("docker", "compose", "-p", project, "down", "-t", "5", cwd=dir_, env=env, timeout=90)
    r.step("compose down", down.returncode == 0, down.stderr)

    up2 = sh("docker", "compose", "-p", project, "up", "-d", cwd=dir_, env=env, timeout=app["up_timeout"] + 30)
    r.step("compose up again", up2.returncode == 0, up2.stderr)
    check_http(r, project, app["http"], app["up_timeout"], "data survived down/up")
    if app["name"] == "migrate-once":
        r.step("marker file survived down/up",
               eventually(30, lambda: "done" in sh("docker", "exec", f"{project}-app-1", "cat", "/shared/migrated").stdout))

    default_recreate(app, r)

    downv = sh("docker", "compose", "-p", project, "down", "-v", "-t", "5", cwd=dir_, env=env, timeout=90)
    r.step("compose down -v", downv.returncode == 0, downv.stderr)

    def nothing_left():
        for kind in ("deploy", "pod", "svc", "pvc", "secret"):
            if project_objects(kind, project):
                return False
        return True

    r.step("every object d2k created is gone (deploy/pod/svc/pvc/secret)", eventually(60, nothing_left))
    return r


def main(argv):
    apps = [a for a in CORPUS if not argv or a["name"] in argv]
    if not apps:
        print("no matching app", file=sys.stderr)
        return 2
    results = [run_app(a) for a in apps]
    print()
    failed = [r.app for r in results if r.failed]
    for r in results:
        print(f"{'FAIL' if r.failed else 'PASS'}  {r.app}")
    if failed:
        print(f"\n{len(failed)} app(s) broke: {', '.join(failed)}")
        return 1
    print(f"\nall {len(results)} app(s) work")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
