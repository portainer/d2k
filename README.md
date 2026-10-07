# d2k — Docker-to-Kubernetes Translator

d2k lets Docker tooling manage a single Kubernetes namespace. It exposes the Docker Engine API over mutual TLS on port 2376 and translates every Docker call into Kubernetes operations scoped to one namespace. Portainer, the Docker CLI, or any Docker SDK client connects to d2k and gets a Kubernetes-backed execution environment without needing to know about Kubernetes.

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
| `docker run` | Deployment, the container's record. `--restart always` and `unless-stopped`: its replica runs the container. `--restart no` (the default) and `on-failure`: a pod created from it runs the container |
| `docker stop` | Scale the Deployment to 0, or delete the pod and record its exit code; returns once the pod is gone |
| `docker start` | Scale the Deployment to 1, or run a new pod |
| `docker restart` | Stop, then start; as in Docker, returns once the container runs again (or fails to), waiting at most 30 s; a client that goes away (Ctrl-C) does not stop it halfway |
| `docker run --rm` | As in Docker, the container is removed when its run ends, once an attached `docker run` has its output; it cannot have a restart policy |
| `docker rm` | Delete the Deployment (and its pod) + Service. As in Docker, a running container needs `-f`, which kills it at once (1 s grace, not its stop timeout) and returns once the pod is gone |
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
| `-u`, `-w`, `-t`, `-i`, `-m`, `--cpus`, `--cpu-shares`, `--memory-reservation`, `--add-host`, `--cap-add/--cap-drop`, `--read-only`, `--privileged`, `--stop-timeout` | Container and pod settings (`-u` must be numeric) |
| `--health-*`, compose `healthcheck:` | Readiness probe; `State.Health` reports starting/healthy/unhealthy. Traffic is not affected, as in Docker. A `HEALTHCHECK` defined only in the image is not seen |
| `--restart` | `always` and `unless-stopped`: the Deployment restarts the container. `no` (the default): the pod's container runs once (`restartPolicy: Never`) and exits with its exit code. `on-failure`: `restartPolicy: OnFailure`; a maximum retry count returns a warning. A pod is not rescheduled if its node fails. A restart the kubelet makes, or a new pod replacing the container's pod (deleted, drained, evicted), is reported as Docker reports a restart: `die`, `restarting` while it waits, then `start`. `docker inspect`'s `RestartCount` is the current pod's container restart count; a pod Kubernetes replaces (deleted by hand, drained, evicted) restarts that count from zero, unlike Docker's, which counts for the container's whole life |
| Any other setting | Ignored, and named in a warning on create (`docker run` prints it) |

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
| VIP endpoint mode | LoadBalancer Service (default) |
| dnsrr endpoint mode | hostPort on container + node IPs returned per running pod |
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

**`docker compose`**: every attribute of the Compose Specification, and what d2k does with it, is listed in [docs/compose-support.md](docs/compose-support.md). Each row is checked against a cluster by `test/e2e/compose_spec.py`.

**`docker stack deploy`** (Swarm mode) supports a standard Compose file. The following documents what is supported, what is partially supported, and what is not.

### Supported

**Services** — all services in a stack are translated to Kubernetes Deployments. `deploy.replicas` is honoured. Services with no explicit `deploy.replicas` default to 1 replica. Image, environment variables, port mappings, and restart policies are all translated correctly.

**Ports** — `ports` mappings create a LoadBalancer Service. Both `<host>:<container>` and short-form syntax are supported.

**dnsrr / host-port mode** — services using `--endpoint-mode dnsrr` or any port with `mode: host` are translated to hostPort bindings on the pod spec rather than a LoadBalancer Service. Pods bind directly on the node's network interface at the published port, and the service endpoint returns the individual node IPs of running pods rather than a VIP. This allows an external load balancer to target cluster nodes directly, bypassing the Kubernetes service routing layer. The endpoint node IP list updates automatically as pods are scheduled or rescheduled.

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

**Resource limits** — `deploy.resources.limits` and `deploy.resources.reservations` are translated to Kubernetes resource limits and requests.

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
- `docker inspect` reports the GPU limit as `HostConfig.DeviceRequests` in the form `--gpus N` sends (`Count=N`, capability `gpu`), whatever form created it, so Portainer's Recreate and Duplicate keep the GPUs.
- The device plugin for your GPU vendor must be installed and healthy on the cluster. d2k does not install or manage it.

---

## Logs

Global mode services (`--mode global`) are deployed as replicated with a warning. Service rollback (`docker service rollback`) is not implemented. Node drain cordon-annotates the node but does not evict existing pods. Port conflict detection across services is not enforced.

---

## Port mapping rules

No `-p` flag means no Service is created. `-P` (publish all) creates a NodePort Service. Explicit `-p host:container` creates a LoadBalancer Service. If a host IP is included (e.g. `-p 127.0.0.1:8080:80`), it is ignored with a warning.

For Swarm services, `--endpoint-mode dnsrr` or any port with `mode: host` bypasses LoadBalancer Service creation entirely. Ports are bound directly on each node via `hostPort`, and the service endpoint returns the node IPs of running pods. Use this when integrating with an external load balancer that needs to target individual nodes directly.

---

## Networking

Kubernetes namespace networking is flat. All Pods in the namespace can reach each other by IP regardless of which Docker network they are assigned to. `docker network create` is accepted and returns a synthetic network, but no actual network isolation is enforced. Networks created by `docker stack deploy` are removed correctly when `docker stack rm` is run.

A Docker network has no Kubernetes object of its own, so d2k records each one, exactly as it was created (name, labels, options), as a `d2k.portainer.io/network-*` annotation on its namespace. A network reports the driver it was created with, `bridge` by default, and Docker's scope for it: `local`, or `swarm` for `overlay`. Networks therefore survive d2k restarts and upgrades. RBAC cannot limit which fields d2k patches on the namespace, so `deploy/kubernetes.yaml` installs a ValidatingAdmissionPolicy that lets d2k's ServiceAccount change only those annotations: it can never change the namespace's labels (such as the `pod-security.kubernetes.io/*` labels Portainer's policies manage), other annotations or spec. **This requires Kubernetes 1.30 or later.**

---

## Volumes

`docker volume create` creates a PersistentVolumeClaim using the cluster's default StorageClass with `ReadWriteOnce` access mode. Size defaults to `1Gi` and can be overridden with `--opt size=5Gi`. The PVC name is the volume name with underscores replaced by hyphens, as Kubernetes requires; Docker clients keep seeing the original name, labels and options, which d2k records on the PVC. Two volume names that map to the same PVC name (`app_data` and `app-data`) cannot both exist. Creating a volume that already exists returns it, as in Docker; a volume created without a name gets a random one.

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

Volumes (PVCs) are not deleted when a stack is removed with `docker stack rm`. This matches Docker Swarm behaviour; named volumes are intentionally preserved across stack teardown to prevent accidental data loss. Data persists and reattaches automatically if the stack is redeployed. To remove a volume explicitly, use `docker volume rm <name>`. As in Docker, `docker volume rm` refuses a volume that a container, running or stopped, still uses (409, "volume is in use"): remove the container first.

The NFS subdirectory created by the CSI provisioner is named after the PVC UID (e.g. `pvc-92f50b38-...`), not the pod. This means pod rescheduling, rolling updates, and replica scaling all reattach to the same data automatically without any intervention.

If your StorageClass has `reclaimPolicy: Delete`, the underlying NFS subdirectory (or other storage backend) will be cleaned up when the PVC is explicitly deleted via `docker volume rm`. It will not be triggered by `docker stack rm` alone.

---

## Deployment

d2k runs inside the target cluster namespace using a ServiceAccount bound to a namespace-scoped Role for workload management and a ClusterRole for node and StorageClass read access.

d2k serves the Docker API over mutual TLS and does not start without its certificates. Create them first, listing every DNS name and IP address clients use to reach d2k:

```bash
d2k pki init --san d2k.d2k.svc.cluster.local,d2k.example.com --out d2k-pki
kubectl create namespace d2k
kubectl -n d2k create secret generic d2k-tls --from-file=d2k-pki/server
kubectl apply -f deploy/kubernetes.yaml
```

`d2k-pki/client` holds the client certificate (`ca.pem`, `cert.pem`, `key.pem`) for the Docker CLI and for Portainer's "Docker API" environment with TLS. Keep `d2k-pki/ca.key` private. `d2k pki init` refuses to overwrite any file it would write, so give it a new `--out` directory to start over. cert-manager can issue the `d2k-tls` Secret instead, as long as it holds `tls.crt`, `tls.key` and the client CA as `ca.crt`. d2k reloads the Secret when it changes, without a restart.

For Swarm mode, set `D2K_SWARM_MODE=true` in the deployment manifest before applying. The single manifest covers both modes; the ClusterRole and ClusterRoleBinding for node and StorageClass access are always included.

### Multiple installs on one cluster

Portainer can point several environments at the same cluster, each as its own d2k install in its own namespace. Everything namespace-scoped (the Role, RoleBinding, ServiceAccount, Deployment, Service, Secret) is already isolated by `namespace: d2k` in the manifest, and the ValidatingAdmissionPolicy is written once and shared safely: it derives the expected ServiceAccount from whichever namespace is being patched, so re-applying it for every install is a no-op after the first.

The ClusterRole and ClusterRoleBinding named `d2k-node-reader` are the exception: they are cluster-scoped, so their names must be made unique per install (for example by appending the namespace, as `test/e2e/setup.sh` and the overnight environment's `install.sh` both do). If two installs keep the default name, the second `kubectl apply` silently overwrites the first's ClusterRoleBinding subject, and the first install's ServiceAccount loses its cluster-wide node list access (`docker info`'s node/manager counts, `docker node ls` in Swarm mode, and `/info`'s leader-election check all stop working, with no error at apply time). Rename both objects (and `roleRef.name`) together, the same way you rename `namespace: d2k`.

Restarting d2k (`kubectl rollout restart`, a pod delete, a reschedule) takes about 5 seconds longer than it used to: a `preStop` hook delays SIGTERM so kube-proxy has time to stop routing to the terminating pod before d2k stops accepting connections. Without it, a client can hit the pod in that window and see its connection reset.

Connect Portainer or the Docker CLI to the d2k Service:

```bash
# Docker CLI
docker context create d2k \
  --docker "host=tcp://d2k.d2k.svc.cluster.local:2376,ca=d2k-pki/client/ca.pem,cert=d2k-pki/client/cert.pem,key=d2k-pki/client/key.pem"

docker --context d2k ps
docker --context d2k service ls
```

---

## Configuration

| Variable | Default | Description |
|---|---|---|
| `D2K_NAMESPACE` | `default` | Target Kubernetes namespace |
| `D2K_TLS_PORT` | `2376` | Docker API port (mutual TLS) |
| `D2K_TLS_CERT_FILE` | `/etc/d2k/tls/tls.crt` | Server certificate |
| `D2K_TLS_KEY_FILE` | `/etc/d2k/tls/tls.key` | Server key |
| `D2K_TLS_CA_FILE` | `/etc/d2k/tls/ca.crt` | CA that client certificates must be signed by |
| `D2K_HEALTH_PORT` | `8081` | Plain HTTP port serving `GET /healthz` only |
| `D2K_SOCKET` | _(empty)_ | Also serve the Docker API on this unix socket (no TLS; file mode 0660) |
| `D2K_INSECURE` | `false` | Serve without TLS or authentication on `D2K_PORT`. Anyone who can reach it controls the namespace |
| `D2K_PORT` | `2375` | Plain Docker API port, used only with `D2K_INSECURE` |
| `D2K_SWARM_MODE` | `false` | Enable Swarm API emulation |
| `D2K_LOG_LEVEL` | `info` | Log level: debug, info, warn, error |
| `D2K_LOG_FORMAT` | `text` | Log format: text, json |
| `D2K_KUBECONFIG` | _(empty)_ | Path to kubeconfig. Empty = in-cluster auth |

---

## Architecture

```
Docker client
     |  Docker Engine API (mutual TLS, port 2376)
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
| pods | get, list, watch, create, delete |
| pods/log | get |
| pods/exec, pods/attach | create |
| services | get, list, watch, create, update, patch, delete |
| configmaps | get, list, watch, create, update, patch, delete |
| secrets | get, list, watch, create, update, patch, delete |
| persistentvolumeclaims | get, list, watch, create, delete |
| namespaces | get, patch _(its own namespace only; an admission policy limits patch to `d2k.portainer.io/network-*` annotations)_ |
| events | get, list |
| resourcequotas | get, list _(a container created without CPU or memory settings gets the requests and limits a quota requires: 10m CPU, 32Mi memory)_ |
| limitranges | get, list _(optional: a LimitRange's container defaults are left to apply instead of d2k's)_ |
| metrics.k8s.io/pods | get, list _(optional)_ |

Swarm mode adds a ClusterRole for node and StorageClass access:

| Resource | Verbs | Scope |
|---|---|---|
| nodes | get, list, watch | ClusterRole |
| storageclasses | get, list, watch | ClusterRole |

Node StorageClass read access is required for NFS volume matching.

---

## Not supported

These features are absent by design. They reflect fundamental differences between the Docker and Kubernetes models and will not be added.

### Image management

`docker build`, `docker commit`, `docker tag`, `docker push`, `docker save`, `docker load`, `docker image prune`, and all `docker buildx` commands are not supported. d2k is a runtime translator. Image build and registry operations operate below the Kubernetes layer — Kubernetes pulls images at schedule time from a registry and has no equivalent concept of a local image daemon. Use a standard CI pipeline and container registry to build and push images before running them through d2k.

d2k stores no images. `docker images` lists none, `docker pull` is acknowledged without pulling, and `docker image inspect` reports only the reference: the kubelet pulls the image when a container starts, and a failed pull shows as the container's state. `docker rmi` is not supported. d2k ignores the registry credentials a client sends (`X-Registry-Auth`); give the namespace pull secrets instead, for example on its default ServiceAccount or through a Portainer policy.

### Container operations

These answer `501 Not Implemented`: `docker kill`, `docker pause` and `docker unpause`, `docker top`, `docker update`, `docker export`, `docker diff`, `docker container prune`, and `docker cp` (which first stats the path in the container; copying into a container through the API, as compose does for secrets and configs, works). Use `docker stop`, `docker rm` and re-create the container instead.

### Advanced networking

`docker network create --driver macvlan` and `--driver ipvlan` are not supported and will not be. macvlan and ipvlan require direct Layer 2 hardware access, assigning real MAC and IP addresses to container interfaces. Kubernetes networking is CNI-managed and operates above this level — there is no Kubernetes equivalent. If your workload requires macvlan (e.g. direct access to a physical LAN segment), it cannot be translated and must remain on a native Docker host.

Additional networking features with no Kubernetes equivalent:

- `--network host` — host network namespace sharing. Use `hostNetwork: true` in a raw Kubernetes manifest instead.
- Per-container `--dns` servers. The cluster DNS server must answer first for service names to resolve. (`--dns-search` and `--dns-option` are added to the pod's DNS configuration.)
- `--ip` and `--mac-address` — static IP and MAC assignment. Not applicable in CNI-managed networking.
- `--link` — legacy Docker container linking. Has no Kubernetes equivalent.

### Other Docker-native features

- `docker checkpoint` / `docker checkpoint restore` — CRIU-based container checkpointing. No Kubernetes equivalent.
- `--privileged` — may be blocked by cluster admission policy (PodSecurity, OPA/Gatekeeper). d2k passes the flag but enforcement is cluster-dependent.
- `--device` — host device passthrough (e.g. `/dev/video0`). Requires a device plugin on Kubernetes; d2k does not configure one.
- `--ulimit` — kernel resource limits. Not expressible in Kubernetes Pod spec.
- `--sysctl` is translated to pod sysctls, but Kubernetes allows only its safe set unless the kubelet allowlists others; a container with a forbidden sysctl fails to start.
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
