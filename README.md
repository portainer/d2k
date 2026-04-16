# d2k — Docker-to-Kubernetes Translator

d2k lets Docker tooling manage a single Kubernetes namespace. It exposes the Docker Engine API on port 2375 and translates every Docker call into Kubernetes operations scoped to one namespace. Portainer, the Docker CLI, or any Docker SDK client connects to d2k and gets a Kubernetes-backed execution environment without needing to know about Kubernetes.

Enable `D2K_SWARM_MODE=true` and the same endpoint emulates a Docker Swarm cluster instead, translating Swarm API calls (services, stacks, tasks, secrets, configs) into Kubernetes Deployments and resources.

---

## Modes

d2k runs in one of two modes, controlled by the `D2K_SWARM_MODE` environment variable.

**Docker host mode** (default) emulates a single Docker Engine. `docker run`, `docker ps`, `docker exec`, and all container-level operations are translated to Kubernetes Deployments and Pods. Portainer connects as a Docker standalone environment.

**Swarm mode** (`D2K_SWARM_MODE=true`) emulates a Docker Swarm cluster. `docker service`, `docker stack`, `docker secret`, and `docker config` operations are translated to Kubernetes resources. Portainer connects as a Swarm environment and renders the full cluster view including nodes, services, stacks, and secrets.

---

## Docker host mode

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

## Swarm mode

| Swarm concept | Kubernetes translation |
|---|---|
| Service | Deployment + optional LoadBalancer Service |
| Task | Pod |
| Stack | Group of Deployments labelled by stack name |
| Secret | Kubernetes Secret |
| Config | Kubernetes ConfigMap |
| Node | Kubernetes Node (read-only) |
| Swarm cluster | Single-namespace, single-node synthetic cluster |
| Overlay network | Synthetic (namespace network is flat) |

Swarm IDs are derived deterministically from Kubernetes UIDs so they are stable across d2k restarts. The cluster identity is stored in a ConfigMap (`d2k-identity`) in the target namespace.

### Tested and confirmed working

`docker service create`, `docker service scale`, `docker service update --image`, and `docker service rm` all converge correctly with the CLI progress bar. `docker service logs` and `docker service logs --follow` stream with correct Docker multiplexed wire format and swarm details context. `docker stack deploy`, `docker stack ls`, `docker stack ps`, and `docker stack rm` all work correctly alongside standalone services. `docker secret` and `docker config` CRUD are fully functional. Portainer renders the Swarm cluster view including nodes, CPU, memory, services, stacks, networks, secrets, and configs.

### Known limitations in Swarm mode

Global mode services (`--mode global`) are deployed as replicated with a warning. Service rollback (`docker service rollback`) is not implemented. Node drain evicts the cordon annotation but does not evict existing pods. Secrets and configs are mounted into service containers via Kubernetes volume mounts but end-to-end injection has not been verified. Port conflict detection across services is not enforced.

---

## Port mapping rules

No `-p` flag means no Service is created. `-P` (publish all) creates a NodePort Service. Explicit `-p host:container` creates a LoadBalancer Service. If a host IP is included (e.g. `-p 127.0.0.1:8080:80`), it is ignored with a warning.

---

## Networking

Kubernetes namespace networking is flat. All Pods in the namespace can reach each other by IP regardless of which Docker network they are assigned to. `docker network create` is accepted and returns a synthetic network, but no actual network isolation is enforced.

---

## Volumes

`docker volume create` creates a PersistentVolumeClaim using the cluster's default StorageClass with `ReadWriteOnce` access mode. Size defaults to `1Gi` and can be overridden with `--opt size=5Gi`. Bind mounts are not supported.

---

## Deployment

d2k runs inside the target cluster namespace using a ServiceAccount with a namespace-scoped Role. It has no cluster-wide permissions except the optional metrics API.

```bash
kubectl apply -f deploy/kubernetes.yaml
```

For Swarm mode, set `D2K_SWARM_MODE=true` in the deployment manifest before applying. A separate manifest (`deploy/sd2k-kubernetes.yaml`) is provided with the correct RBAC for Swarm mode, which additionally requires node read access via a ClusterRole.

Connect Portainer or the Docker CLI to the d2k Service:

```bash
# Docker CLI
docker context create d2k \
  --docker "host=tcp://d2k.d2k.svc.cluster.local:2375"

docker --context d2k ps
docker --context d2k service ls
```

---

## Configuration

| Variable | Default | Description |
|---|---|---|
| `D2K_NAMESPACE` | `default` | Target Kubernetes namespace |
| `D2K_PORT` | `2375` | Docker API listen port |
| `D2K_SWARM_MODE` | `false` | Enable Swarm API emulation |
| `D2K_LOG_LEVEL` | `info` | Log level: debug, info, warn, error |
| `D2K_LOG_FORMAT` | `text` | Log format: text, json |
| `D2K_KUBECONFIG` | _(empty)_ | Path to kubeconfig. Empty = in-cluster auth |

---

## Architecture

```
Docker client
     |  Docker Engine API (port 2375)
     v
+------------------------------------------+
|  d2k                                     |
|                                          |
|  router         <- HTTP ServeMux         |
|  |                                       |
|  +-- /containers/*                       |
|  +-- /volumes/*                          |
|  +-- /networks/*  (label filter aware)   |
|  +-- /images/*                           |
|  +-- /exec/*                             |
|  +-- /events                             |
|  +-- /swarm, /nodes                      |
|  +-- /services/*, /tasks/*               |
|  +-- /stacks/*, /secrets/*, /configs/*   |
|  +-- /_ping, /version, /info             |
|                                          |
|  adapter        <- translation layer     |
|  |                                       |
|  +-- container.go   (Deployments)        |
|  +-- swarm.go       (Swarm surface)      |
|  +-- volume.go      (PVCs)               |
|  +-- network.go     (synthetic)          |
|  +-- logs.go        (pod log stream)     |
|  +-- exec.go        (pod exec/SPDY)      |
|  +-- metrics.go     (metrics-server)     |
|  +-- events.go      (resource watch)     |
+------------------------------------------+
     |  Kubernetes API (in-cluster)
     v
Kubernetes namespace
```

---

## RBAC

Docker host mode requires namespace-scoped permissions only.

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
| metrics.k8s.io/pods | get, list _(optional)_ |

Swarm mode adds:

| Resource | Verbs | Scope |
|---|---|---|
| nodes | get, list, watch, update, patch | ClusterRole |
| secrets | get, list, watch, create, update, patch, delete | namespace |
| configmaps | get, list, watch, create, update, patch, delete | namespace |

---

## Limitations

- `docker commit`, `docker save`, and `docker load` are not supported.
- Bind mounts are not supported in either mode.
- Network isolation is not enforced. All pods share the namespace network.
- Image metadata is synthesised. Actual image metadata lives on cluster nodes.
- `docker stats` requires metrics-server to return real data.
- Swarm mode is single-namespace and single-node. Multi-node scheduling constraints are accepted but ignored.
