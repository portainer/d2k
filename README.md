# d2k — Docker-to-Kubernetes Translator

d2k is the reverse of [k2d](https://github.com/portainer/k2d). Where k2d lets
Kubernetes tooling manage a Docker host, d2k lets Docker tooling manage a single
Kubernetes namespace.

Run d2k inside your cluster. It exposes the Docker Engine API on port 2375 and
translates every Docker call into Kubernetes operations scoped to one namespace.
Portainer, the Docker CLI, or any Docker SDK client connects to d2k and gets a
Kubernetes-backed execution environment without needing to know about Kubernetes.

---

## How it works

| Docker concept | Kubernetes translation |
|---|---|
| Container | Deployment (replicas=1) |
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

### Port mapping rules

- **No `-p` flag** → no Service created
- **`-P` (auto-publish all exposed ports)** → NodePort Service
- **`-p <host>:<container>` (explicit mapping)** → LoadBalancer Service

If a host IP is included (e.g. `-p 127.0.0.1:8080:80`), it is ignored with a
warning — there is no Kubernetes equivalent of binding to a specific node interface.

### Networking

Kubernetes namespace networking is flat. All Pods in the namespace can reach each
other by IP regardless of which Docker network they are assigned to. `docker network
create` is accepted and returns a synthetic network ID, but no actual network
isolation is enforced. A warning is logged when a network other than the defaults
is created.

### Volumes

`docker volume create` creates a PersistentVolumeClaim using the cluster's default
StorageClass with `ReadWriteOnce` access mode. Size defaults to `1Gi` and can be
overridden with `--opt size=5Gi`.

Bind mounts (`-v /host/path:/container/path`) are not supported and will be
rejected. Named volume mounts (`-v myvolume:/data`) map to PVC mounts.

---

## Deployment

d2k runs inside the target cluster namespace using a ServiceAccount with a
namespace-scoped Role. It has no cluster-wide permissions.

```bash
# Edit deploy/kubernetes.yaml to set your target namespace, then:
kubectl apply -f deploy/kubernetes.yaml
```

Connect Portainer or the Docker CLI to the d2k Service:

```bash
# Docker CLI context
docker context create d2k \
  --docker "host=tcp://d2k.default.svc.cluster.local:2375"

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
| `D2K_LOW_PORT_THRESHOLD` | `1024` | Ports below this use LoadBalancer (not used currently — all explicit mappings use LB) |

`D2K_KUBECONFIG` should not be set when running inside the cluster. The in-cluster
ServiceAccount token is used automatically.

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
│  └── /_ping, /version, /info        │
│                                     │
│  adapter       ← translation layer  │
│  │                                  │
│  ├── container.go  (Deployments)    │
│  ├── volume.go     (PVCs)           │
│  ├── network.go    (synthetic)      │
│  └── logs.go       (Pod log stream) │
└─────────────────────────────────────┘
     │  Kubernetes API (in-cluster)
     ▼
Kubernetes namespace
```

---

## Limitations

- Replicas are always 1. d2k is designed for single-instance workloads.
- `docker exec` is not yet implemented (requires SPDY upgrade handling).
- `docker commit` / `docker save` / `docker load` are not supported.
- Bind mounts are not supported.
- Image metadata is synthesised — `docker image inspect` returns minimal data.
- Network isolation between Docker networks is not enforced.
- `docker stats` is not implemented.

---

## Relation to k2d

k2d and d2k are complementary translators:

- **k2d**: runs on a Docker host, accepts Kubernetes API calls, executes Docker operations. Designed for resource-constrained Industrial IoT devices.
- **d2k**: runs in a Kubernetes cluster, accepts Docker API calls, executes Kubernetes operations. Designed for Docker-native tooling (Portainer) managing a namespace.

k2d is now archived in favour of [KubeSolo](https://github.com/portainer/kubesolo).
