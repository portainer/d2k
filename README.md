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
| `--gpus all` / `--gpus N` | Pod resource limits via device plugin (opt-in) |

---

## Swarm mode

| Swarm concept | Kubernetes translation |
|---|---|
| Service | Deployment + optional LoadBalancer Service |
| Task | Pod |
| Stack | Group of Deployments labelled by stack name |
| Secret | Kubernetes Secret |
| Config | Kubernetes ConfigMap |
| Node | Kubernetes Node |
| Manager node | Control-plane node (`node-role.kubernetes.io/control-plane` or `master`) |
| Worker node | Non-control-plane node |
| Swarm leader | Control-plane node serving the Kubernetes API (matched by IP) |
| Overlay network | Synthetic (namespace network is flat) |

Swarm IDs are derived deterministically from Kubernetes UIDs so they are stable across d2k restarts. The cluster identity is stored in a ConfigMap (`d2k-identity`) in the target namespace.

Node count, manager count, and the Swarm leader are derived from the live Kubernetes cluster on every `/info` call. Multi-node clusters are represented correctly. `docker swarm init` and `docker swarm leave` return `501 Not Implemented` — d2k is a translator, not a real Swarm node.

### Tested and confirmed working

`docker service create`, `docker service scale`, `docker service update --image`, and `docker service rm` all converge correctly with the CLI progress bar. `docker service logs` and `docker service logs --follow` stream with correct Docker multiplexed wire format and swarm details context. `docker stack deploy`, `docker stack ls`, `docker stack ps`, and `docker stack rm` all work correctly alongside standalone services. `docker secret` and `docker config` CRUD are fully functional. Portainer renders the Swarm cluster view including nodes, CPU, memory, services, stacks, networks, secrets, and configs.

---

## Docker Compose and docker stack deploy

d2k supports `docker stack deploy` using a standard Compose file. The following documents what is supported, what is partially supported, and what is not.

### Supported

**Services** — all services in a stack are translated to Kubernetes Deployments. `deploy.replicas` is honoured. Services with no explicit `deploy.replicas` default to 1 replica. Image, environment variables, port mappings, and restart policies are all translated correctly.

**Ports** — `ports` mappings create a LoadBalancer Service. Both `<host>:<container>` and short-form syntax are supported.

**Secrets** — secrets defined in the `secrets:` block and referenced by services are created as Kubernetes Secrets and mounted into containers at `/run/secrets/<name>`. Secret names containing underscores (e.g. `my_secret`) are automatically sanitised to hyphen form for Kubernetes compatibility (`my-secret`) and resolved back transparently when the Docker CLI references them by the original name. `docker stack deploy` is idempotent for secrets — re-deploying a stack does not error if the secret already exists. `docker secret create` on an already-existing secret returns an error as expected.

**Configs** — configs defined in the `configs:` block and referenced by services are created as Kubernetes ConfigMaps and mounted into containers. The same name sanitisation and idempotency behaviour as secrets applies.

**Networks** — networks defined in the `networks:` block are created as synthetic overlay networks. Kubernetes namespace networking is flat so no real network isolation is enforced, but the network names resolve correctly for DNS within the namespace. Networks are removed correctly when `docker stack rm` is run.

**Volumes (standard)** — named volumes without driver options are created as PersistentVolumeClaims against the cluster's default StorageClass with `ReadWriteOnce` access mode and a default size of 1Gi. Volumes are mounted into service containers at the path specified in the service `volumes:` entry.

**Volume mounts** — `volumes:`, `bind` mounts, and `tmpfs` mounts in the service spec are all translated: named volumes become PVC references, bind mounts become hostPath volumes, and tmpfs mounts become emptyDir with memory medium.

**Service inspection by ID** — `docker service inspect <ID>` works with the full ID, any unique prefix (matching Docker CLI behaviour), or the service name.

**Uptime formatting** — `docker ps` STATUS column shows human-readable uptime (e.g. `8 days`) rather than raw duration.

### Partially supported

**NFS volumes** — volumes with `driver_opts.type: nfs` are supported when the cluster has `nfs.csi.k8s.io` installed with a StorageClass whose `parameters.server` matches the `addr=` value in the compose `driver_opts.o`. d2k locates the matching StorageClass automatically and creates a `ReadWriteMany` PVC using it. If no matching StorageClass is found, the deploy is blocked with an error indicating exactly what is required. Other NFS provisioners (e.g. `nfs-subdir-external-provisioner`) are not supported for automatic matching because their server address is not stored in the StorageClass spec.

**Global mode services** — `deploy.mode: global` is deployed as replicated with a warning. True DaemonSet translation is not implemented.

**Health checks** — `healthcheck:` in a service spec is not translated to a Kubernetes readiness or liveness probe.

**Resource limits** — `deploy.resources.limits` and `deploy.resources.reservations` are not currently translated to Kubernetes resource requests and limits.

**Dependencies** — `depends_on:` has no Kubernetes equivalent and is ignored. Kubernetes does not guarantee pod start order; use readiness probes in your application instead.

### Not supported

**Build directives** — `build:` in a service spec is ignored. d2k is a runtime translator; images must be pre-built and pushed to a registry before deployment.

**`extends`** — Compose file extension and merging is handled by the Docker CLI before the API call reaches d2k; d2k sees only the resolved spec.

**`--compatibility` flag** — Compose v2 compatibility mode flags are not acted on.

**Bind mounts from host paths** — `volumes:` entries using host path syntax (e.g. `./data:/app/data`) are translated to hostPath volumes. This will only work if the path exists on the Kubernetes node where the pod is scheduled, which is generally unreliable in a multi-node cluster.

**macvlan / ipvlan networks** — not supported. See the Networking section.

---

## GPU support

d2k can translate Docker's `--gpus` flag into Kubernetes device plugin resource requests. Support is opt-in via the `D2K_GPU_RESOURCE_NAME` environment variable — when unset, GPU flags are silently ignored for backward compatibility.

Set `D2K_GPU_RESOURCE_NAME` to the resource name advertised by the device plugin installed on your cluster:

| GPU vendor | Typical resource name |
|---|---|
| NVIDIA | `nvidia.com/gpu` |
| AMD | `amd.com/gpu` |
| Intel | `intel.com/gpu` or `gpu.intel.com/i915` |

To verify what your nodes advertise:

```bash
kubectl get nodes -o json | jq '.items[].status.allocatable' | grep -Ei 'gpu|neuron|tpu'
```

Once configured, `docker run --gpus all` or `--gpus 2` will create a Deployment with the matching resource limit on the container spec, and the Kubernetes scheduler will place the pod on a GPU node.

```bash
docker --context d2k run -d --gpus all --name ml-job pytorch/pytorch:latest
```

### Notes and limitations

- Docker sends `--gpus all` as `Count=-1`. d2k treats this as `1` since Kubernetes device plugins require an explicit count. Use `--gpus N` to request more.
- Only one resource name is supported per d2k instance. Clusters with mixed GPU vendors need separate d2k deployments.
- Fractional GPUs, MIG slicing, and vendor-specific capability selectors are not parsed — d2k only emits integer counts against a single resource name.
- The device plugin for your GPU vendor must be installed and healthy on the cluster. d2k does not install or manage it.

---

## Logs

Global mode services (`--mode global`) are deployed as replicated with a warning. Service rollback (`docker service rollback`) is not implemented. Node drain cordon-annotates the node but does not evict existing pods. Port conflict detection across services is not enforced.

---

## Port mapping rules

No `-p` flag means no Service is created. `-P` (publish all) creates a NodePort Service. Explicit `-p host:container` creates a LoadBalancer Service. If a host IP is included (e.g. `-p 127.0.0.1:8080:80`), it is ignored with a warning.

---

## Networking

Kubernetes namespace networking is flat. All Pods in the namespace can reach each other by IP regardless of which Docker network they are assigned to. `docker network create` is accepted and returns a synthetic network, but no actual network isolation is enforced. Networks created by `docker stack deploy` are removed correctly when `docker stack rm` is run.

---

## Volumes

`docker volume create` creates a PersistentVolumeClaim using the cluster's default StorageClass with `ReadWriteOnce` access mode. Size defaults to `1Gi` and can be overridden with `--opt size=5Gi`. Volume names containing underscores are sanitised to hyphens to comply with Kubernetes naming rules.

### NFS volumes

NFS volumes defined in a Compose file using `driver_opts` are supported when the cluster has the `nfs.csi.k8s.io` CSI driver installed with a StorageClass whose `parameters.server` matches the `addr=` value in the compose `driver_opts.o`.

Example Compose volume definition:

```yaml
volumes:
  db-data:
    driver_opts:
      type: "nfs"
      o: "addr=192.168.1.4,nolock,soft,rw"
      device: ":/mnt/nfs/data"
```

Example matching StorageClass:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: nfs-csi
provisioner: nfs.csi.k8s.io
parameters:
  server: 192.168.1.4
  share: /mnt/nfs/data
reclaimPolicy: Delete
volumeBindingMode: Immediate
mountOptions:
  - nfsvers=3
  - nolock
```

At startup, d2k probes the cluster for `nfs.csi.k8s.io` StorageClasses and logs the NFS servers it will support:

```
INFO  NFS volume support available  storageClass=nfs-csi  server=192.168.1.4
```

When a stack service references an NFS volume, d2k creates a `ReadWriteMany` PVC using the matched StorageClass. The NFS CSI provisioner then creates a subdirectory on the share named after the PVC UID (e.g. `pvc-92f50b38-...`) and mounts it into the pod.

If no matching StorageClass is found, the deploy is blocked with an error:

```
NFS volume "db-data": no StorageClass with provisioner "nfs.csi.k8s.io" and server="192.168.1.4" found in cluster
```

Note: only `nfs.csi.k8s.io` is supported for automatic matching. Other NFS provisioners such as `nfs-subdir-external-provisioner` are not supported because their server address is not stored in the StorageClass spec.

### Volume lifecycle

Volumes (PVCs) are not deleted when a stack is removed with `docker stack rm`. This matches Docker Swarm behaviour — named volumes are intentionally preserved across stack teardown to prevent accidental data loss. Data persists and reattaches automatically if the stack is redeployed. To remove a volume explicitly, use `docker volume rm <name>`.

The NFS subdirectory created by the CSI provisioner is named after the PVC UID (e.g. `pvc-92f50b38-...`), not the pod. This means pod rescheduling, rolling updates, and replica scaling all reattach to the same data automatically without any intervention.

If your StorageClass has `reclaimPolicy: Delete`, the underlying NFS subdirectory (or other storage backend) will be cleaned up when the PVC is explicitly deleted via `docker volume rm`. It will not be triggered by `docker stack rm` alone.

---

## Deployment

d2k runs inside the target cluster namespace using a ServiceAccount bound to a namespace-scoped Role for workload management and a ClusterRole for node and StorageClass read access.

```bash
kubectl apply -f deploy/kubernetes.yaml
```

For Swarm mode, set `D2K_SWARM_MODE=true` in the deployment manifest before applying. The single manifest covers both modes — the ClusterRole and ClusterRoleBinding for node and StorageClass access are always included.

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
| configmaps | get, list, watch, create, update, patch, delete |
| secrets | get, list, watch, create, update, patch, delete |
| persistentvolumeclaims | get, list, watch, create, delete |
| namespaces | get |
| events | get, list |
| metrics.k8s.io/pods | get, list _(optional)_ |

Swarm mode adds a ClusterRole for node and StorageClass access:

| Resource | Verbs | Scope |
|---|---|---|
| nodes | get, list, watch, update, patch | ClusterRole |
| storageclasses | get, list, watch | ClusterRole |

Node update/patch is required for `docker node update` (drain/active/pause), which cordon-annotates the Kubernetes node. StorageClass read access is required for NFS volume matching.

---

## Not supported

These features are absent by design. They reflect fundamental differences between the Docker and Kubernetes models and will not be added.

### Image management

`docker build`, `docker commit`, `docker tag`, `docker push`, `docker save`, `docker load`, `docker image prune`, and all `docker buildx` commands are not supported. d2k is a runtime translator. Image build and registry operations operate below the Kubernetes layer — Kubernetes pulls images at schedule time from a registry and has no equivalent concept of a local image daemon. Use a standard CI pipeline and container registry to build and push images before running them through d2k.

### Advanced networking

`docker network create --driver macvlan` and `--driver ipvlan` are not supported and will not be. macvlan and ipvlan require direct Layer 2 hardware access, assigning real MAC and IP addresses to container interfaces. Kubernetes networking is CNI-managed and operates above this level — there is no Kubernetes equivalent. If your workload requires macvlan (e.g. direct access to a physical LAN segment), it cannot be translated and must remain on a native Docker host.

Additional networking features with no Kubernetes equivalent:

- `--network host` — host network namespace sharing. Use `hostNetwork: true` in a raw Kubernetes manifest instead.
- Per-container `--dns` and `--dns-search` overrides. DNS in Kubernetes is cluster-wide and namespace-scoped via CoreDNS.
- `--ip` and `--mac-address` — static IP and MAC assignment. Not applicable in CNI-managed networking.
- `--link` — legacy Docker container linking. Has no Kubernetes equivalent.

### Other Docker-native features

- `docker checkpoint` / `docker checkpoint restore` — CRIU-based container checkpointing. No Kubernetes equivalent.
- `--privileged` — may be blocked by cluster admission policy (PodSecurity, OPA/Gatekeeper). d2k passes the flag but enforcement is cluster-dependent.
- `--device` — host device passthrough (e.g. `/dev/video0`). Requires a device plugin on Kubernetes; d2k does not configure one.
- `--ulimit` — kernel resource limits. Not expressible in Kubernetes Pod spec.
- `--sysctl` — kernel parameter overrides. Supported only for a restricted set by Kubernetes; d2k does not translate these.
- `docker system prune` and `docker system df` — operate against a local Docker daemon. Not meaningful in a Kubernetes context.

---

## Limitations

- Bind mounts from arbitrary host paths are unreliable in multi-node clusters.
- Network isolation is not enforced. All pods share the namespace network.
- Image metadata is synthesised. Actual image metadata lives on cluster nodes.
- `docker stats` requires metrics-server to return real data.
- Swarm services are scoped to a single namespace. Multi-namespace deployments require separate d2k instances.
- `docker swarm init` and `docker swarm leave` return `501 Not Implemented`.
- Global mode services (`--mode global`) are deployed as replicated with a warning.
- NFS volumes require `nfs.csi.k8s.io` with a matching StorageClass. Other NFS provisioners are not auto-detected.
