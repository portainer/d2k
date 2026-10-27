# End-to-end tests

Black-box tests that drive d2k on a real Kubernetes cluster with the Docker
CLI and Docker Compose, and check the resulting cluster state with kubectl.

## Prerequisites

`kind`, `kubectl`, `docker` (with the Compose plugin), `curl`, `perl`,
Go 1.26 or later (`setup.sh` runs `go run ./cmd pki init` to create the test
certificates) and `python3` (`run.sh`'s terminal checks and `compose_spec.py`).

Create the cluster with both d2k NodePorts mapped to the host:

```sh
cat <<EOF | kind create cluster --name d2k --wait 1m --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraPortMappings:
  - containerPort: 31111
    hostPort: 31111
    protocol: TCP
  - containerPort: 31112
    hostPort: 31112
    protocol: TCP
EOF
```

## Run

```sh
test/e2e/setup.sh   # build d2k:dev from the working tree, load it, deploy
test/e2e/run.sh     # run the suite
```

`compose_spec.py` checks every attribute of the Compose Specification with the
real `docker compose up` (about 3 minutes; it needs `python3`):

```sh
test/e2e/compose_spec.py                 # every attribute
test/e2e/compose_spec.py ports tmpfs     # some attributes
test/e2e/compose_spec.py --doc           # regenerate docs/compose-support.md
```

Each case in its `CASES` table is one attribute: its support level, the note
published in `docs/compose-support.md`, and what compose must do with it (work,
with a check of the resulting Deployment; warn; or fail). Change the table when
d2k's behaviour changes, then regenerate the document.

`setup.sh` applies `deploy/admission-policy.yaml` once and deploys two instances from `deploy/kubernetes.yaml`:

| Mode       | Namespace   | Endpoint                |
|------------|-------------|-------------------------|
| standalone | `d2k`       | `tcp://localhost:31112` |
| swarm      | `d2k-swarm` | `tcp://localhost:31111` |

Re-run `setup.sh` after code changes; it rebuilds the image and restarts both
instances. Set `CLUSTER` to use a kind cluster with another name.

## Upgrades and installs

These build their own images, install d2k into their own namespaces of the
kind cluster (`CLUSTER`, default `d2k`), check it as a Docker client would,
and remove what they created. They need no `setup.sh`.

```sh
test/e2e/upgrade-1.2.sh                          # 1.2.3 -> working tree, Docker host mode
SWARM=true test/e2e/upgrade-1.2.sh               # 1.2.3 in Swarm mode, kept
SWARM=true AFTER=false test/e2e/upgrade-1.2.sh   # Swarm install upgraded to host mode
FROM=<git ref> test/e2e/upgrade.sh               # an earlier mTLS build -> working tree
test/e2e/install-lifecycle.sh                    # README install twice, uninstall one
test/e2e/portainer.sh                            # d2k driven through Portainer CE
```

`upgrade-1.2.sh` follows README's "Upgrading from d2k 1.2.x": the new
manifest is applied with no `d2k-tls` Secret, so d2k creates its
certificates, and the checks use the client certificate fetched from
`d2k-client`. `upgrade.sh` keeps the admin's own certificates and checks that
containers, volumes, networks and compose projects come through unchanged.
`install-lifecycle.sh` checks that removing one install leaves the shared
admission policy, and the other install, working. `portainer.sh` runs
Portainer CE (`PORTAINER_IMAGE`, default `portainer/portainer-ce:lts`) next to
d2k, adds d2k as a "Docker API" environment over mutual TLS at its in-cluster
address, and makes the calls Portainer's UI makes: containers, volumes,
networks and a compose stack. Calls d2k doesn't support (kill, pause, top,
system df) must answer 501; Portainer hides them for d2k.

## Results

- `PASS` / `FAIL`: the case's expectation held or did not.
- `XFAIL`: a known gap, tagged with the phase that fixes it.
- `XPASS`: a known gap now works; turn the case into a regular `check`.

`run.sh` exits non-zero on any `FAIL` or `XPASS`. It deletes every object d2k
created (`d2k.portainer.io/managed-by=d2k`) before and after the run.
