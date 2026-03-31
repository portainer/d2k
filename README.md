# d2k — Docker-to-Kubernetes Translator

d2k lets Docker tooling manage a single Kubernetes namespace.

Run d2k inside your cluster. It exposes the Docker Engine API on port 2375 and translates every Docker call into Kubernetes operations scoped to one namespace. Portainer, the Docker CLI, or any Docker SDK client connects to d2k and gets a Kubernetes-backed execution environment without needing to know about Kubernetes.

---

## How it works

| Docker concept | Kubernetes translation |
|---|---|
| `docker run` | Deployment (replicas=1) |
| `docker stop` | Scale Deployment to 0 |
| `docker start` | Scale Deployment to 1 |
| `docker rm` | Delete Deployment + Service |
| `-p <host>:<container>` | LoadBalancer Service |
| `-P` (publish all) | NodePort Service |
| No port flags | No Service created |
| `docker volume create` | PersistentVolumeClaim |
| `docker network create` | Synthetic (namespace network is flat) |
| `docker pull` | Acknowledged — Kubernetes pulls at schedule time |
| `docker logs` | Kubernetes pod log stream |
| `docker exec` | Kubernetes pod exec via SPDY |
| `docker stats` | Kubernetes metrics API (falls back to zeroes if unavailable) |
| `docker events` | Kubernetes resource watch + event history |

---

## Port mapping rules

No `-p` flag means no Service is created. `-P` (publish all) creates a NodePort Service. Explicit `-p host:container` creates a LoadBalancer Service.

If a host IP is included (e.g. `-p 127.0.0.1:8080:80`), it is ignored with a warning — there is no Kubernetes equivalent of binding to a specific node interface.

---

## Networking

Kubernetes namespace networking is flat. All Pods in the namespace can reach each other by IP regardless of which Docker network they are assigned to. `docker network create` is accepted and returns a synthetic network ID, but no actual network isolation is enforced. A warning is logged when a network other than the defaults is created.

---

## Volumes

`docker volume create` creates a PersistentVolumeClaim using the cluster's default StorageClass with `ReadWriteOnce` access mode. Size defaults to `1Gi` and can be overridden with `--opt size=5Gi`.

Bind mounts (`-v /host/path:/container/path`) are not supported. Named volume mounts (`-v myvolume:/data`) map to PVC mounts.

---

## Logs

`docker logs` streams from the Kubernetes pod log API. Both `--follow` and `--tail` are supported. Output is multiplexed in Docker wire format so the CLI and Portainer both receive it correctly.

---

## Exec and console

`docker exec` is implemented via the Kubernetes SPDY exec API. Interactive TTY sessions (`docker exec -it`) work from the Docker CLI. Portainer's browser-based container console connects via WebSocket and is fully supported.

---

## Stats

`docker stats` streams container CPU and memory metrics. If the Kubernetes metrics-server is available in the cluster, d2k uses it to return real usage data. If not, it returns zeroed stats without erroring. d2k probes for metrics-server availability at startup and logs which mode it is running in.

---

## Events

`docker events` streams Kubernetes resource changes translated to Docker event format. Historical events (from the Kubernetes Events API, retained for approximately one hour by default) are replayed first, then the live watch stream begins. Supported event types:

| Kubernetes resource | Docker event |
|---|---|
| Pod created | container start |
| Pod deleted | container stop |
| Deployment created | container create |
| Deployment deleted | container destroy |
| PVC created | volume create |
| PVC deleted | volume destroy |
| Service created | network connect |
| Service deleted | network disconnect |

---

## Deployment

d2k runs inside the target cluster namespace using a ServiceAccount with a namespace-scoped Role. It has no cluster-wide permissions (except the optional metrics API, which requires a ClusterRole if metrics-server is installed cluster-wide).

```bash
# Edit deploy/kubernetes.yaml to set your target namespace, then:
kubectl apply -f deploy/kubernetes.yaml
```

Connect Portainer or the Docker CLI to the d2k Service:

```bash
# Docker CLI context
docker context create d2k \
  --docker "host=tcp://d2k.d2k.svc.cluster.local:2375"

docker --context d2k ps
docker --context d2k run -d --name myapp -p 80:8080 nginx
```

---

## Configuration

All configuration is via environment variables.

| Variable | Default | Description |
|---|---|---|
| `D2K_NAMESPACE` | `default` | Target Kubernetes namespace |
| `D2K_PORT` | `2375` | Docker API listen port |
| `D2K_LOG_LEVEL` | `info` | Log level: debug, info, warn, error |
| `D2K_LOG_FORMAT` | `text` | Log format: text, json |
| `D2K_KUBECONFIG` | _(empty)_ | Path to kubeconfig. Empty = in-cluster auth |
| `D2K_LOW_PORT_THRESHOLD` | `1024` | Reserved, not currently used |

`D2K_KUBECONFIG` should not be set when running inside the cluster. The in-cluster ServiceAccount token is used automatically.

---

## Architecture

```
Docker client
     │  Docker Engine API (port 2375)
     ▼
┌─────────────────────────────────────┐
│  d2k                                │
│                                     │
│  router        ← HTTP ServeMux      │
│  │                                  │
│  ├── /containers/*                  │
│  ├── /volumes/*                     │
│  ├── /networks/*                    │
│  ├── /images/*                      │
│  ├── /exec/*                        │
│  ├── /events                        │
│  └── /_ping, /version, /info        │
│                                     │
│  adapter       ← translation layer  │
│  │                                  │
│  ├── container.go  (Deployments)    │
│  ├── volume.go     (PVCs)           │
│  ├── network.go    (synthetic)      │
│  ├── logs.go       (pod log stream) │
│  ├── exec.go       (pod exec/SPDY)  │
│  ├── metrics.go    (metrics-server) │
│  └── events.go     (resource watch) │
└─────────────────────────────────────┘
     │  Kubernetes API (in-cluster)
     ▼
Kubernetes namespace
```

---

## RBAC

d2k requires the following permissions in the target namespace. The supplied `deploy/kubernetes.yaml` includes a Role with these rules pre-configured.

| Resource | Verbs |
|---|---|
| deployments | get, list, watch, create, update, patch, delete |
| pods | get, list, watch |
| pods/log | get |
| pods/exec | create |
| services | get, list, watch, create, update, patch, delete |
| persistentvolumeclaims | get, list, watch, create, delete |
| namespaces | get |
| events | get, list |
| metrics.k8s.io/pods | get, list _(optional, for docker stats)_ |

The metrics.k8s.io rule is optional. If omitted, `docker stats` returns zeroes rather than real usage data.

---

## Limitations

- Replicas are always 1. d2k is designed for single-instance workloads.
- `docker commit`, `docker save`, and `docker load` are not supported.
- Bind mounts are not supported. Named volume mounts map to PVCs.
- Image metadata is synthesised — `docker image inspect` returns minimal data. Actual image metadata lives on cluster nodes.
- Network isolation between Docker networks is not enforced. All pods share the namespace network.
- The Kubernetes Cluster running d2k should be equipped with a Load Balancer, so d2k can expose containers on low ports as per docker standard.
- `-P` (publish all) requires the Docker client to supply `ExposedPorts` in the create request. If the client has not pulled the image locally, `ExposedPorts` will be empty and no Service will be created.
- `docker stats` requires metrics-server to return real data. Without it, all metrics return zero.

---
