# End-to-end tests

Black-box tests that drive d2k on a real Kubernetes cluster with the Docker
CLI and Docker Compose, and check the resulting cluster state with kubectl.

## Prerequisites

`kind`, `kubectl`, `docker` (with the Compose plugin), `curl` and `perl`.

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

`setup.sh` deploys two instances from `deploy/kubernetes.yaml`:

| Mode       | Namespace   | Endpoint                |
|------------|-------------|-------------------------|
| standalone | `d2k`       | `tcp://localhost:31112` |
| swarm      | `d2k-swarm` | `tcp://localhost:31111` |

Re-run `setup.sh` after code changes; it rebuilds the image and restarts both
instances. Set `CLUSTER` to use a kind cluster with another name.

## Results

- `PASS` / `FAIL`: the case's expectation held or did not.
- `XFAIL`: a known gap, tagged with the phase that fixes it.
- `XPASS`: a known gap now works; turn the case into a regular `check`.

`run.sh` exits non-zero on any `FAIL` or `XPASS`. It deletes every object d2k
created (`d2k.portainer.io/managed-by=d2k`) before and after the run.
