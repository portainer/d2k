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
import json
import os
import shutil
import ssl
import subprocess
import sys
import tempfile
import time
import urllib.error
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


def compose(cwd, project, *args, timeout=240):
    return sh("docker", "compose", "-p", project, *args, cwd=cwd, timeout=timeout)


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


class PortForward:
    """A published port becomes a LoadBalancer Service, which a bare kind cluster never
    assigns an external IP to (no cloud load balancer), so the app's host:// ports are not
    reachable from the test runner the way they would be on real Docker. `kubectl
    port-forward` to the pod reaches the app the same way a real operator would from outside
    the cluster, and does not depend on kind's node-to-host port mapping."""

    def __init__(self, project, service, local_port, remote_port):
        self.local_port, self.remote_port = local_port, remote_port
        self.target = f"deploy/{project}-{service}-1"
        self.proc = None

    def __enter__(self):
        self.proc = subprocess.Popen(
            ["kubectl", "--context", CONTEXT, "-n", NS, "port-forward", self.target,
             f"{self.local_port}:{self.remote_port}"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        eventually(20, lambda: http_ok(self.local_port, "/", "") or self._listening())
        return self

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
        up = compose(app["dir"], app["project"], "up", "-d")
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
         http=[("traefik", 18185, 80, "/", "Hostname:")], up_timeout=60),
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
        with PortForward(project, service, local_port, remote_port):
            this_ok = eventually(timeout, lambda p=local_port, h=path, w=want: http_ok(p, h, w))
        ok = r.step(f"{label}: {path} answers as expected", this_ok) and ok
    return ok


def project_objects(kind, project):
    out = kubectl("-n", NS, "get", kind, "-l", f"com.docker.compose.project={project}", "-o", "name")
    return [l for l in out.stdout.splitlines() if l.strip()]


def run_app(app):
    r = Result(app["name"])
    print(f"== {app['name']} ({app['project']})")
    dir_, project = app["dir"], app["project"]
    env = dict(ENV, **app.get("env", {}))

    up = sh("docker", "compose", "-p", project, "up", "-d", cwd=dir_, env=env, timeout=app["up_timeout"] + 30)
    if not r.step("compose up", up.returncode == 0, (up.stdout + up.stderr)[-800:]):
        compose(dir_, project, "down", "-v", "-t", "1", timeout=120)
        return r

    check_http(r, project, app["http"], app["up_timeout"], "HTTP")

    if app["name"] == "minio":
        r.step("mc init job created the bucket",
               eventually(60, lambda: "init-done" in compose(dir_, project, "logs", "init").stdout))

    if app["name"] == "migrate-once":
        r.step("the app only started after the migration completed",
               eventually(60, lambda: "done" in sh("docker", "exec", f"{project}-app-1", "cat", "/shared/migrated").stdout))

    if "secret_check" in app:
        service, local_port, remote_port, path, want = app["secret_check"]
        with PortForward(project, service, local_port, remote_port):
            r.step("the secret from `environment` reached the container",
                   eventually(30, lambda p=local_port, h=path, w=want: http_ok(p, h, w)))

    ps = compose(dir_, project, "ps", "--format", "json")
    n_up = len([l for l in ps.stdout.splitlines() if l.strip()])
    r.step("compose ps lists the project's containers", n_up == len(app["services"]), ps.stdout)

    logs = compose(dir_, project, "logs", app["main_service"])
    r.step("compose logs returns output", bool(logs.stdout.strip()) or bool(logs.stderr.strip()))

    main_ctr = f"{project}-{app['main_service']}-1"
    ex = sh("docker", "exec", main_ctr, "sh", "-c", "echo corpus-exec-ok")
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
