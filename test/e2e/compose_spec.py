#!/usr/bin/env python3
"""Compose Specification conformance for d2k.

Every attribute of the Compose Specification (https://github.com/compose-spec/compose-spec)
has a row in CASES: how d2k supports it, a fixture, and what `docker compose up`
must do with it against d2k. A row either works (and its Kubernetes result is
checked), warns, or fails with an error: no attribute may be dropped silently.

    compose_spec.py              run every case against the d2k from setup.sh
    compose_spec.py ports tmpfs  run the named cases
    compose_spec.py --doc        write docs/compose-support.md from CASES
"""
import json
import os
import shlex
import subprocess
import sys
import tempfile
from concurrent.futures import ThreadPoolExecutor

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
ENV = dict(os.environ, DOCKER_HOST=os.environ.get("STANDALONE", "tcp://localhost:31112"), DOCKER_TLS_VERIFY="1",
           DOCKER_CERT_PATH=os.environ.get("PKI", os.path.join(HERE, ".pki", "client")), DB_PASS="s3cret")
CONTEXT = os.environ.get("CONTEXT", "kind-d2k")
# COMPOSE_CMD picks the compose binary/plugin invocation: "docker compose" (default, the
# CLI plugin) or a standalone binary such as a pinned docker-compose release, e.g.
# COMPOSE_CMD=/path/to/docker-compose-v2.40.3 (no "compose" subcommand for a standalone binary).
COMPOSE = shlex.split(os.environ.get("COMPOSE_CMD", "docker compose"))

YES, PARTIAL, NO, CLIENT = "Supported", "Partial", "Not supported", "Compose"

# Companion services a fixture can add.
DB = "  db:\n    image: redis:7-alpine\n"
DB_HEALTHY = "  db:\n    image: redis:7-alpine\n    healthcheck: {test: [CMD, redis-cli, ping], interval: 2s}\n"
INIT_JOB = "  init:\n    image: busybox\n    command: ['true']\n    restart: 'no'\n"


def ctr(deps):
    return deps[0]["spec"]["template"]["spec"]["containers"][0]


def pod(deps):
    return deps[0]["spec"]["template"]["spec"]


def ok(check=None):
    return ("ok", check)


def warns(text):
    return ("warn", text)


def fails(text):
    return ("error", text)


def case(attr, support, note, service, expect, top="", extra=""):
    return dict(attr=attr, support=support, note=note, service=service, expect=expect, top=top, extra=extra)


# The section a case belongs to in the generated document is the part of its
# attribute name before the first dot.
CASES = [
    # --- services: process -------------------------------------------------
    case("image", YES, "", "", ok(lambda d: ctr(d)["image"] == "nginx:alpine")),
    case("command", YES, "Becomes the container's `args`.", "command: [sleep, '3600']",
         ok(lambda d: ctr(d)["args"] == ["sleep", "3600"])),
    case("command.string", YES, "See `command`. A string is split shell-style by compose before it reaches d2k.",
         "command: sleep 3600", ok(lambda d: ctr(d)["args"] == ["sleep", "3600"])),
    case("entrypoint", YES, "Becomes the container's `command`.", "entrypoint: [sh, -c, 'sleep 3600']",
         ok(lambda d: ctr(d)["command"] == ["sh", "-c", "sleep 3600"])),
    case("entrypoint.string", YES, "See `entrypoint`. A string is split shell-style by compose before it reaches d2k.",
         "entrypoint: /bin/sh", ok(lambda d: ctr(d)["command"] == ["/bin/sh"])),
    case("environment", YES, "", "environment: {A: '1'}",
         ok(lambda d: {"name": "A", "value": "1"} in ctr(d)["env"])),
    case("environment.list", YES, "See `environment`. The list form.", "environment: ['A=1']",
         ok(lambda d: {"name": "A", "value": "1"} in ctr(d)["env"])),
    case("environment.value_syntax", YES, "See `environment`. A value may itself contain `=` and spaces.",
         "environment: ['A=x=y z']", ok(lambda d: {"name": "A", "value": "x=y z"} in ctr(d)["env"])),
    case("env_file", CLIENT, "Read by compose; arrives as `environment`.", "env_file: [./app.env]",
         ok(lambda d: {"name": "FROM_FILE", "value": "1"} in ctr(d)["env"])),
    case("env_file.syntax", CLIENT, "See `env_file`. Comments and quoted values are stripped by compose before they "
         "reach d2k.", "env_file: [./app2.env]",
         ok(lambda d: {"name": "QUOTED", "value": "has spaces"} in ctr(d)["env"])),
    case("working_dir", YES, "", "working_dir: /srv", ok(lambda d: ctr(d)["workingDir"] == "/srv")),
    case("user", PARTIAL, "Numeric `uid[:gid]` becomes `runAsUser`/`runAsGroup`. User names need the image's "
         "`/etc/passwd`, which Kubernetes does not read: they are ignored with a warning.", "user: '1000:1000'",
         ok(lambda d: ctr(d)["securityContext"]["runAsUser"] == 1000)),
    case("user.name", PARTIAL, "See `user`.", "user: nginx", warns('user "nginx" ignored')),
    case("tty", YES, "", "tty: true", ok(lambda d: ctr(d).get("tty") is True)),
    case("stdin_open", YES, "", "stdin_open: true", ok(lambda d: ctr(d).get("stdin") is True)),
    case("stop_grace_period", YES, "Becomes `terminationGracePeriodSeconds`.", "stop_grace_period: 20s",
         ok(lambda d: pod(d)["terminationGracePeriodSeconds"] == 20)),
    case("stop_signal", NO, "Kubernetes always stops containers with SIGTERM.", "stop_signal: SIGINT", warns("StopSignal")),
    case("init", NO, "Kubernetes has no init process injection.", "init: true", warns("HostConfig.Init")),
    case("healthcheck", PARTIAL, "Becomes a readiness probe, which `docker ps` and `depends_on: service_healthy` "
         "read. `start_interval` is ignored with a warning: probes have one period.",
         "healthcheck: {test: [CMD, 'true'], interval: 5s, timeout: 3s, retries: 4, start_period: 10s}",
         ok(lambda d: ctr(d)["readinessProbe"]["periodSeconds"] == 5 and ctr(d)["readinessProbe"]["failureThreshold"] == 4)),
    case("healthcheck.start_interval", PARTIAL, "See `healthcheck`.",
         "healthcheck: {test: [CMD, 'true'], start_period: 10s, start_interval: 1s}", warns("start interval ignored")),
    case("healthcheck.test_string", PARTIAL, "See `healthcheck`. A string `test` runs under `CMD-SHELL`.",
         "healthcheck: {test: 'true', interval: 5s}", ok(lambda d: ctr(d)["readinessProbe"]["periodSeconds"] == 5)),
    case("healthcheck.test_shell", PARTIAL, "See `healthcheck`.", "healthcheck: {test: [CMD-SHELL, 'true'], interval: 5s}",
         ok(lambda d: ctr(d)["readinessProbe"]["periodSeconds"] == 5)),
    case("restart", YES, "`always` and `unless-stopped` containers run as a Deployment's replica, which restarts. "
         "`no` (the default) runs the container once in a pod (`restartPolicy: Never`), and `on-failure` in a pod with "
         "`restartPolicy: OnFailure`; a maximum retry count is ignored with a warning. Such pods are not rescheduled "
         "if their node fails.", "restart: always", ok(lambda d: d[0]["metadata"]["annotations"]["d2k.portainer.io/restart-policy"] == "always")),
    case("restart.no", YES, "See `restart`.", "restart: 'no'",
         ok(lambda d: d[0]["metadata"]["annotations"]["d2k.portainer.io/restart-policy"] == "no")),
    case("restart.on_failure_max", PARTIAL, "See `restart`.", "restart: on-failure:3", warns("maximum retry count 3 ignored")),
    case("restart.unless_stopped", YES, "See `restart`.", "restart: unless-stopped",
         ok(lambda d: d[0]["metadata"]["annotations"]["d2k.portainer.io/restart-policy"] == "unless-stopped")),
    case("post_start", PARTIAL, "Run with exec once the container runs. Hooks with a `user`, `working_dir`, "
         "`environment` or `privileged` fail: Kubernetes exec runs as the container's process does.",
         "post_start: [{command: [touch, /tmp/x]}]", ok()),
    case("post_start.user", PARTIAL, "See `post_start`.", "post_start: [{command: [touch, /tmp/x], user: root}]",
         fails("exec with a custom user is not supported")),
    case("pre_start", NO, "Not yet in a compose release.", "", None),
    case("pre_stop", PARTIAL, "Run with exec before the container stops; same limits as `post_start`.",
         "pre_stop: [{command: [touch, /tmp/x]}]", ok()),

    # --- services: identity and metadata -----------------------------------
    case("container_name", YES, "The Docker name is kept; the Kubernetes name is derived from it.",
         "container_name: my-web", ok(lambda d: d[0]["metadata"]["annotations"]["d2k.portainer.io/docker-name"] == "my-web")),
    case("labels", YES, "Kept verbatim; also set as Kubernetes labels where the value is valid.", "labels: {a.b: c}",
         ok(lambda d: d[0]["metadata"]["labels"]["a.b"] == "c")),
    case("labels.list", YES, "See `labels`. The list form.", "labels: ['a.b=c']",
         ok(lambda d: d[0]["metadata"]["labels"]["a.b"] == "c")),
    case("labels.long_key", YES, "See `labels`. A key over 63 characters is kept verbatim on the Docker label but is "
         "not a valid Kubernetes label key, so it is not also set there.",
         "labels: {this-key-is-seventy-characters-long-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx: c}",
         ok(lambda d: "this-key-is-seventy-characters-long-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" not in d[0]["metadata"]["labels"])),
    case("label_file", CLIENT, "Read by compose; arrives as `labels`.", "label_file: ./app.labels",
         ok(lambda d: d[0]["metadata"]["labels"]["from.file"] == "1")),
    case("annotations", PARTIAL, "Set on the pod. Container runtimes receive pod annotations only where configured to.",
         "annotations: {com.example.a: b}", ok(lambda d: d[0]["spec"]["template"]["metadata"]["annotations"]["com.example.a"] == "b")),
    case("hostname", NO, "A compose service's pod hostname is its service name, which service DNS needs: ignored with a "
         "warning. (`docker run --hostname` sets the pod hostname.)", "hostname: myhost", warns('hostname "myhost" ignored')),
    case("domainname", NO, "", "domainname: example.com", warns("Domainname")),
    case("mac_address", NO, "The cluster network assigns pod addresses.", "mac_address: 02:42:ac:11:00:02", warns("MacAddress")),
    case("platform", NO, "The node's container runtime picks the image platform.", "platform: linux/arm64", warns('platform "linux/arm64"')),
    case("pull_policy", CLIENT, "Compose decides whether to pull; d2k's pull is a no-op and the kubelet pulls the image "
         "(`IfNotPresent`).", "pull_policy: always", ok()),
    case("pull_refresh_after", CLIENT, "As `pull_policy`.", "pull_policy: daily", ok()),
    case("profiles", CLIENT, "", "profiles: [debug]", ("absent", None), extra=DB),
    case("extends", CLIENT, "", "extends: {file: ./base.yaml, service: base}",
         ok(lambda d: {"name": "BASE", "value": "1"} in ctr(d)["env"])),
    case("attach", CLIENT, "", "attach: false", ok()),

    # --- services: dependencies and scaling ---------------------------------
    case("depends_on", YES, "", "depends_on: [db]", ok(), extra=DB),
    case("depends_on.service_started", YES, "The map form with the default condition.",
         "depends_on: {db: {condition: service_started}}", ok(), extra=DB),
    case("depends_on.service_healthy", YES, "Waits on the dependency's healthcheck.",
         "depends_on: {db: {condition: service_healthy}}", ok(), extra=DB_HEALTHY),
    case("depends_on.service_completed_successfully", YES, "The dependency runs once (`restart: \"no\"`) and "
         "compose waits for it to exit with code 0.", "depends_on: {init: {condition: service_completed_successfully}}",
         ok(), extra=INIT_JOB),
    case("scale", CLIENT, "Compose creates one container per replica.", "scale: 2", ("count", 2)),
    case("deploy.replicas", CLIENT, "As `scale`.", "deploy: {replicas: 2}", ("count", 2)),
    case("deploy.resources", YES, "Limits and reservations of CPU and memory become limits and requests.",
         "deploy: {resources: {limits: {cpus: '0.5', memory: 64M}, reservations: {memory: 32M}}}",
         ok(lambda d: ctr(d)["resources"] == {"limits": {"cpu": "500m", "memory": "64Mi"}, "requests": {"memory": "32Mi"}})),
    case("deploy.resources.limits_only", YES, "See `deploy.resources`.", "deploy: {resources: {limits: {memory: 64M}}}",
         ok(lambda d: ctr(d)["resources"] == {"limits": {"memory": "64Mi"}})),
    case("deploy.resources.reservations_only", CLIENT, "See `deploy.resources`. Compose does not forward a CPU "
         "reservation outside Swarm mode; only `reservations.memory` reaches d2k.",
         "deploy: {resources: {reservations: {cpus: '0.25', memory: 32M}}}",
         ok(lambda d: ctr(d)["resources"] == {"requests": {"memory": "32Mi"}})),
    case("deploy.restart_policy", YES, "As `restart`.", "deploy: {restart_policy: {condition: on-failure}}",
         ok(lambda d: d[0]["metadata"]["annotations"]["d2k.portainer.io/restart-policy"] == "on-failure")),
    case("deploy.placement", CLIENT, "Ignored by compose outside Swarm mode.", "deploy: {placement: {constraints: [node.role==manager]}}", ok()),

    # --- services: resources ------------------------------------------------
    case("cpus", YES, "CPU limit.", "cpus: 0.5", ok(lambda d: ctr(d)["resources"]["limits"]["cpu"] == "500m")),
    case("cpu_quota", YES, "With `cpu_period`, a CPU limit.", "cpu_period: 100000\n    cpu_quota: 50000",
         ok(lambda d: ctr(d)["resources"]["limits"]["cpu"] == "500m")),
    case("cpu_period", YES, "See `cpu_quota`.", "cpu_period: 100000\n    cpu_quota: 25000",
         ok(lambda d: ctr(d)["resources"]["limits"]["cpu"] == "250m")),
    case("cpu_shares", YES, "A CPU request (1024 shares = 1 CPU).", "cpu_shares: 512",
         ok(lambda d: ctr(d)["resources"]["requests"]["cpu"] == "500m")),
    case("mem_limit", YES, "Memory limit.", "mem_limit: 64m", ok(lambda d: ctr(d)["resources"]["limits"]["memory"] == "64Mi")),
    case("mem_reservation", YES, "Memory request.", "mem_reservation: 32m",
         ok(lambda d: ctr(d)["resources"]["requests"]["memory"] == "32Mi")),
    case("gpus", PARTIAL, "A GPU limit on the cluster's GPU resource (`nvidia.com/gpu` or `amd.com/gpu`); `all` "
         "requests one. Ignored with a warning when no node offers GPUs.", "gpus: all", warns("GPU request ignored")),
    case("cpu_count", NO, "Windows only.", "cpu_count: 2", warns("CpuCount")),
    case("cpu_percent", NO, "Windows only.", "cpu_percent: 50", warns("CpuPercent")),
    case("cpu_rt_period", NO, "", "cpu_rt_period: 1000000", warns("CpuRealtimePeriod")),
    case("cpu_rt_runtime", NO, "", "cpu_rt_runtime: 95000", warns("CpuRealtimeRuntime")),
    case("cpuset", NO, "Use the kubelet's CPU manager instead.", "cpuset: '0'", warns("CpusetCpus")),
    case("memswap_limit", NO, "", "memswap_limit: 128m\n    mem_limit: 64m", warns("MemorySwap")),
    case("mem_swappiness", NO, "", "mem_swappiness: 10", warns("MemorySwappiness")),
    case("oom_kill_disable", NO, "", "oom_kill_disable: true", warns("OomKillDisable")),
    case("oom_score_adj", NO, "The kubelet sets it from the pod's QoS class.", "oom_score_adj: 500", warns("OomScoreAdj")),
    case("pids_limit", NO, "A kubelet setting, not a pod one.", "pids_limit: 100", warns("PidsLimit")),
    case("blkio_config", NO, "", "blkio_config: {weight: 300}", warns("BlkioWeight")),
    case("ulimits", NO, "Kubernetes has no per-container ulimits.", "ulimits: {nofile: {soft: 1024, hard: 2048}}", warns("Ulimits")),
    case("shm_size", YES, "A memory-backed `emptyDir` at `/dev/shm` with that size limit.", "shm_size: 128m",
         ok(lambda d: {"medium": "Memory", "sizeLimit": "128Mi"} in [v.get("emptyDir") for v in pod(d)["volumes"]])),

    # --- services: security -------------------------------------------------
    case("cap_add", YES, "", "cap_add: [NET_ADMIN]", ok(lambda d: ctr(d)["securityContext"]["capabilities"]["add"] == ["NET_ADMIN"])),
    case("cap_drop", YES, "", "cap_drop: [ALL]", ok(lambda d: ctr(d)["securityContext"]["capabilities"]["drop"] == ["ALL"])),
    case("privileged", YES, "Subject to the namespace's pod security policy.", "privileged: true",
         ok(lambda d: ctr(d)["securityContext"]["privileged"] is True)),
    case("read_only", YES, "", "read_only: true", ok(lambda d: ctr(d)["securityContext"]["readOnlyRootFilesystem"] is True)),
    case("security_opt", PARTIAL, "`no-new-privileges` sets `allowPrivilegeEscalation: false`; `seccomp=unconfined`, "
         "`apparmor=<profile>` and `label=user|role|type|level:<value>` set the matching profiles and SELinux options. "
         "Seccomp profile files and other options are ignored with a warning: Kubernetes loads profiles from the node.",
         "security_opt: [no-new-privileges:true, 'label=type:spc_t']",
         ok(lambda d: ctr(d)["securityContext"]["allowPrivilegeEscalation"] is False and ctr(d)["securityContext"]["seLinuxOptions"]["type"] == "spc_t")),
    case("security_opt.seccomp_profile", PARTIAL, "See `security_opt`.", "security_opt: ['seccomp=./profile.json']",
         warns("seccomp profile ignored")),
    case("group_add", PARTIAL, "Numeric groups become `supplementalGroups`; group names are ignored with a warning.",
         "group_add: ['1001']", ok(lambda d: pod(d)["securityContext"]["supplementalGroups"] == [1001])),
    case("sysctls", YES, "Pod `sysctls`. Kubernetes allows its safe sysctls; others need the kubelet's allowlist, or "
         "the container fails to start with an error.", "sysctls: {net.ipv4.ip_unprivileged_port_start: '0'}",
         ok(lambda d: pod(d)["securityContext"]["sysctls"] == [{"name": "net.ipv4.ip_unprivileged_port_start", "value": "0"}])),
    case("userns_mode", NO, "", "userns_mode: host", warns("UsernsMode")),
    case("credential_spec", NO, "Windows only; compose does not send it to a Linux engine.", "credential_spec: {file: x.json}", ok()),
    case("isolation", NO, "Windows only.", "isolation: process", warns("Isolation")),
    case("runtime", NO, "Not yet translated.", "runtime: runc", warns("HostConfig.Runtime")),

    # --- services: host namespaces and devices ------------------------------
    case("pid", NO, "", "pid: host", warns("PidMode")),
    case("ipc", NO, "", "ipc: host", warns("IpcMode")),
    case("uts", NO, "", "uts: host", warns("UTSMode")),
    case("cgroup", NO, "", "cgroup: host", warns("CgroupnsMode")),
    case("cgroup_parent", NO, "", "cgroup_parent: m-executor", warns("CgroupParent")),
    case("devices", NO, "Use a device plugin and resource requests.", "devices: ['/dev/null:/dev/xnull']", warns("HostConfig.Devices")),
    case("device_cgroup_rules", NO, "", "device_cgroup_rules: ['c 1:3 mr']", warns("DeviceCgroupRules")),
    case("storage_opt", NO, "", "storage_opt: {size: 1G}", warns("StorageOpt")),
    case("use_api_socket", NO, "There is no Docker socket to share: compose's bind mount of it is ignored with a warning.",
         "use_api_socket: true", warns("host paths are not supported")),

    # --- services: networking -----------------------------------------------
    case("ports", PARTIAL, "Published ports get a LoadBalancer Service. A port with no published port is "
         "published on its container port. A `host_ip` is ignored with a warning.",
         "ports: ['18081:80']", ok(lambda d: ctr(d)["ports"] == [{"containerPort": 80, "protocol": "TCP"}])),
    case("ports.range", YES, "", "ports: ['18090-18091:80-81']", ok(lambda d: len(ctr(d)["ports"]) == 2)),
    case("ports.host_ip", PARTIAL, "See `ports`.", "ports: [{target: 80, published: '18082', protocol: udp, host_ip: 127.0.0.1}]",
         warns('host IP "127.0.0.1"')),
    case("ports.container_only", PARTIAL, "See `ports`. A bare container port with no host port published.",
         "ports: ['80']", ok(lambda d: ctr(d)["ports"] == [{"containerPort": 80, "protocol": "TCP"}])),
    case("ports.udp", PARTIAL, "See `ports`.", "ports: ['18083:80/udp']",
         ok(lambda d: ctr(d)["ports"] == [{"containerPort": 80, "protocol": "UDP"}])),
    case("ports.dual_protocol", PARTIAL, "See `ports`. The same port published for both protocols.",
         "ports: ['18084:80/tcp', '18084:80/udp']",
         ok(lambda d: sorted(p["protocol"] for p in ctr(d)["ports"]) == ["TCP", "UDP"])),
    case("ports.long_syntax", PARTIAL, "See `ports`. The long syntax; `mode: host` has no Kubernetes equivalent "
         "(every published port already reaches every node) and is ignored.",
         "ports: [{target: 80, published: 18085, protocol: tcp, mode: host}]",
         ok(lambda d: ctr(d)["ports"] == [{"containerPort": 80, "protocol": "TCP"}])),
    case("ports.wildcard_host", PARTIAL, "See `ports`. `host_ip: 0.0.0.0` with no host port: a random host port, "
         "explicitly bound to every interface.", "ports: ['0.0.0.0::80']",
         ok(lambda d: ctr(d)["ports"] == [{"containerPort": 80, "protocol": "TCP"}])),
    case("expose", YES, "The ports are reachable within the namespace.", "expose: ['8080']", ok()),
    case("networks", PARTIAL, "Containers resolve the services of their compose project by name. The namespace network "
         "is flat: networks do not isolate containers.", "networks: [front]", ok(), top="networks: {front: {}}"),
    case("networks.map", PARTIAL, "See `networks`. The map form with no per-network settings.",
         "networks: {front: null}", ok(), top="networks: {front: {}}"),
    case("networks.aliases", NO, "Containers resolve by compose service name only.", "networks: {front: {aliases: [www]}}",
         warns('network alias "www" ignored'), top="networks: {front: {}}"),
    case("networks.ipv4_address", NO, "The cluster network assigns pod addresses.", "networks: {front: {ipv4_address: 172.30.0.10}}",
         warns("IPAMConfig"), top="networks: {front: {ipam: {config: [{subnet: 172.30.0.0/24}]}}}"),
    case("networks.mac_address", NO, "", "networks: {front: {mac_address: 02:42:ac:11:00:02}}", warns("MacAddress"),
         top="networks: {front: {}}"),
    case("networks.priority", NO, "", "networks: {front: {gw_priority: 5}}", warns("GwPriority"), top="networks: {front: {}}"),
    case("network_mode", NO, "`host`, `none`, `service:` and `container:` modes: the container joins the namespace network.",
         "network_mode: host", warns('network mode "host"')),
    case("network_mode.service", NO, "See `network_mode`.", "network_mode: service:db", warns("network mode"), extra=DB),
    case("links", NO, "Use the service name, which resolves within the project.", "links: [db]", warns("HostConfig.Links"), extra=DB),
    case("external_links", NO, "", "external_links: [other:alias]", warns("HostConfig.Links")),
    case("extra_hosts", YES, "Becomes `hostAliases`. `host-gateway` is ignored with a warning.",
         "extra_hosts: ['db.local=10.0.0.5']", ok(lambda d: pod(d)["hostAliases"] == [{"hostnames": ["db.local"], "ip": "10.0.0.5"}])),
    case("dns", NO, "The cluster DNS server must answer first for service names to resolve: ignored with a warning.",
         "dns: [1.1.1.1]", warns("DNS servers 1.1.1.1 ignored")),
    case("dns_opt", YES, "Added to the pod's DNS options, after d2k's own.", "dns_opt: [use-vc]",
         ok(lambda d: {"name": "use-vc"} in pod(d)["dnsConfig"]["options"])),
    case("dns_search", YES, "Searched after the compose project's and the namespace's domains.", "dns_search: [example.com]",
         ok(lambda d: pod(d)["dnsConfig"]["searches"][-1] == "example.com")),

    # --- services: storage --------------------------------------------------
    case("volumes", PARTIAL, "Named volumes are PersistentVolumeClaims; `tmpfs` mounts are memory-backed `emptyDir`s. "
         "Bind, `image` and `npipe` mounts are ignored with a warning: pods cannot reach the client's files. "
         "Anonymous volumes (`volumes: [/data]`) are ignored with a warning: their data stays in the container.", "volumes: [data:/data]",
         ok(lambda d: pod(d)["volumes"][0]["persistentVolumeClaim"]["claimName"].endswith("data")), top="volumes: {data: {}}"),
    case("volumes.anonymous", NO, "See `volumes`.", "volumes: [/data]", warns('anonymous volume at "/data" ignored')),
    case("volumes.bind", NO, "See `volumes`.", "volumes: [./src:/src]", warns("host paths are not supported")),
    case("volumes.bind_absolute", NO, "See `volumes`. An absolute client path.", "volumes: [/tmp/src:/src]",
         warns("host paths are not supported")),
    case("volumes.read_only", YES, "The long syntax's `read_only` becomes the mount's `readOnly`.",
         "volumes: [{type: volume, source: data, target: /data, read_only: true}]",
         ok(lambda d: ctr(d)["volumeMounts"][0]["readOnly"] is True), top="volumes: {data: {}}"),
    case("volumes.tmpfs", YES, "A memory-backed `emptyDir`, with `tmpfs.size` as its size limit.",
         "volumes: [{type: tmpfs, target: /cache, tmpfs: {size: 1048576}}]",
         ok(lambda d: {"medium": "Memory", "sizeLimit": "1Mi"} in [v.get("emptyDir") for v in pod(d)["volumes"]])),
    case("volumes.image", NO, "", "volumes: [{type: image, source: busybox, target: /img}]", warns('image mount at "/img" ignored')),
    case("volumes.subpath", YES, "Becomes the mount's `subPath`.",
         "volumes: [{type: volume, source: data, target: /data, volume: {subpath: sub}}]",
         ok(lambda d: ctr(d)["volumeMounts"][0]["subPath"] == "sub"), top="volumes: {data: {}}"),
    case("volumes.nocopy", YES, "d2k never copies image content into a new volume, so every volume behaves as `nocopy`.",
         "volumes: [{type: volume, source: data, target: /data, volume: {nocopy: true}}]", ok(), top="volumes: {data: {}}"),
    case("volumes_from", NO, "", "volumes_from: [db]", warns("VolumesFrom"), extra=DB),
    case("tmpfs", PARTIAL, "Memory-backed `emptyDir`s; a `size=` option is the size limit, other options are ignored "
         "with a warning.", "tmpfs: ['/run:size=64m']",
         ok(lambda d: {"medium": "Memory", "sizeLimit": "64Mi"} in [v.get("emptyDir") for v in pod(d)["volumes"]])),

    # --- services: secrets and configs --------------------------------------
    case("secrets", PARTIAL, "Secrets from `environment` (and configs from `content` or `environment`) are stored in "
         "a Secret owned by the container and mounted read-only at their target with their `mode`; `uid` and `gid` are "
         "not kept, and they total 1 MiB at most. Secrets from a `file` are ignored with a warning: the file is on the "
         "client.", "secrets: [pw]", ok(lambda d: any(m["mountPath"] == "/run/secrets/pw" and m["readOnly"] for m in ctr(d)["volumeMounts"])), top="secrets: {pw: {environment: DB_PASS}}"),
    case("secrets.file", NO, "See `secrets`.", "secrets: [pw]", warns('"/run/secrets/pw" ignored'), top="secrets: {pw: {file: ./pw.txt}}"),
    case("secrets.long_syntax", PARTIAL, "See `secrets`. The long syntax's `target` and `mode`.",
         "secrets: [{source: pw, target: db_pw, mode: 0400}]",
         ok(lambda d: any(m["mountPath"] == "/run/secrets/db_pw" and m["readOnly"] for m in ctr(d)["volumeMounts"])),
         top="secrets: {pw: {environment: DB_PASS}}"),
    case("configs", PARTIAL, "As `secrets`.", "configs: [cfg]", ok(lambda d: any(m["mountPath"] == "/cfg" and m["readOnly"] for m in ctr(d)["volumeMounts"])), top="configs: {cfg: {content: hello}}"),
    case("configs.file", NO, "See `configs`.", "configs: [cfg]", warns('"/cfg" ignored'), top="configs: {cfg: {file: ./cfg.txt}}"),
    case("configs.long_syntax", PARTIAL, "See `configs`. The long syntax's `target` and `mode`.",
         "configs: [{source: cfg, target: /etc/app/cfg, mode: 0400}]",
         ok(lambda d: any(m["mountPath"] == "/etc/app/cfg" and m["readOnly"] for m in ctr(d)["volumeMounts"])),
         top="configs: {cfg: {content: hello}}"),

    # --- services: logging --------------------------------------------------
    case("logging", PARTIAL, "Logs are read from Kubernetes. Drivers other than `json-file` and log options are "
         "ignored with a warning: the kubelet rotates logs.", "logging: {driver: syslog}", warns('log driver "syslog"')),
    case("logging.options", PARTIAL, "See `logging`.", "logging: {driver: json-file, options: {max-size: 10m}}", warns("log options max-size")),

    # --- services: Docker-only tooling --------------------------------------
    case("build", NO, "d2k cannot build images: build and push them, then use `image`.", "", None),
    case("develop", NO, "`compose watch` needs a writable container filesystem and builds.", "", None),
    case("models", NO, "Needs Docker Model Runner.", "", None),
    case("provider", CLIENT, "Runs a provider plugin on the client.", "", None),

    # --- top-level networks -------------------------------------------------
    case("networks.top.internal", NO, "Not enforced; reported on the containers that join the network.",
         "networks: [back]", warns("internal is not enforced"), top="networks: {back: {internal: true}}"),
    case("networks.top.ipam", NO, "Reported on the containers that join the network.", "networks: [back]",
         warns("IPAM settings are ignored"), top="networks: {back: {ipam: {config: [{subnet: 172.31.0.0/24}]}}}"),
    case("networks.top.driver", NO, "`macvlan` and `ipvlan` are refused: pods have no Layer 2 attachment.",
         "networks: [lan]", fails("macvlan"), top="networks: {lan: {driver: macvlan}}"),
    case("networks.top.enable_ipv6", NO, "Pods get the cluster's address families; reported on joining containers.",
         "networks: [back]", warns("IPv6 is not configured"), top="networks: {back: {enable_ipv6: true}}"),
    case("networks.top.enable_ipv4", NO, "IPv4 cannot be disabled; reported on joining containers.",
         "networks: [back]", warns("IPv4 cannot be disabled"), top="networks: {back: {enable_ipv4: false, enable_ipv6: true}}"),
    case("networks.top.driver_opts", NO, "Reported on the containers that join the network.", "networks: [back]",
         warns("driver options are ignored"), top="networks: {back: {driver_opts: {com.docker.network.bridge.name: br9}}}"),
    case("networks.top.attachable", YES, "Every container can join every network.", "networks: [back]", ok(),
         top="networks: {back: {attachable: true}}"),
    case("networks.top.labels", YES, "", "networks: [back]", ok(), top="networks: {back: {labels: {a.b: c}}}"),
    case("networks.top.name", YES, "", "networks: [back]", ok(), top="networks: {back: {name: custom-net}}"),
    case("networks.top.external", YES, "", "networks: [ext]", fails("could not be found"),
         top="networks: {ext: {external: true, name: nosuchnet}}"),

    # --- top-level volumes --------------------------------------------------
    case("volumes.top.labels", YES, "Name, labels and driver options are kept verbatim.", "volumes: [data:/data]", ok(),
         top="volumes: {data: {labels: {a.b: c}}}"),
    case("volumes.top.driver", NO, "Volumes are PersistentVolumeClaims of the default StorageClass; NFS `driver_opts` "
         "select an NFS CSI StorageClass. Other drivers are refused.", "volumes: [data:/data]", fails("volume driver"),
         top="volumes: {data: {driver: rexray}}"),
    case("volumes.top.driver_opts", PARTIAL, "`size` sets the claim's size; NFS options (`type: nfs`, `o: addr=...`) "
         "select an NFS CSI StorageClass. Others are kept on the volume.", "volumes: [data:/data]", ok(),
         top="volumes: {data: {driver_opts: {size: 2Gi}}}"),
    case("volumes.top.name", YES, "", "volumes: [data:/data]", ok(), top="volumes: {data: {name: custom_vol}}"),
    case("volumes.top.external", YES, "", "volumes: [data:/data]", fails("not found"),
         top="volumes: {data: {external: true, name: nosuchvol}}"),
    case("secrets.top.environment", PARTIAL, "See `secrets`.", "secrets: [pw]", ok(lambda d: any(m["mountPath"] == "/run/secrets/pw" and m["readOnly"] for m in ctr(d)["volumeMounts"])),
         top="secrets: {pw: {environment: DB_PASS}}"),
    case("secrets.top.file", NO, "See `secrets`.", "secrets: [pw]", warns('"/run/secrets/pw" ignored'), top="secrets: {pw: {file: ./pw.txt}}"),
    case("secrets.top.driver", CLIENT, "Compose rejects secret drivers outside Swarm mode.", "secrets: [pw]",
         fails("does not support secrets.*.driver"), top="secrets: {pw: {file: ./pw.txt, driver: vault}}"),
    case("secrets.top.template_driver", CLIENT, "As `driver`.", "secrets: [pw]",
         fails("does not support secrets.*.template_driver"), top="secrets: {pw: {file: ./pw.txt, template_driver: golang}}"),
    case("configs.top.content", PARTIAL, "See `secrets`.", "configs: [cfg]", ok(lambda d: any(m["mountPath"] == "/cfg" and m["readOnly"] for m in ctr(d)["volumeMounts"])),
         top="configs: {cfg: {content: hello}}"),
    case("configs.top.environment", PARTIAL, "See `secrets`.", "configs: [cfg]", ok(lambda d: any(m["mountPath"] == "/cfg" and m["readOnly"] for m in ctr(d)["volumeMounts"])),
         top="configs: {cfg: {environment: DB_PASS}}"),
    case("configs.top.file", NO, "See `configs`.", "configs: [cfg]", warns('"/cfg" ignored'), top="configs: {cfg: {file: ./cfg.txt}}"),
    case("configs.top.external", CLIENT, "Compose rejects external configs outside Swarm mode.", "configs: [cfg]",
         fails("external config"), top="configs: {cfg: {external: true}}"),
    case("secrets.top.external", CLIENT, "Compose rejects external secrets outside Swarm mode.", "secrets: [pw]",
         fails("unsupported external secret"), top="secrets: {pw: {external: true}}"),
]

FILES = {
    "app.env": "FROM_FILE=1\n", "app2.env": "# a comment\nQUOTED=\"has spaces\"\n",
    "profile.json": '{"defaultAction": "SCMP_ACT_ALLOW"}', "app.labels": "from.file=1\n", "cfg.txt": "cfg\n", "pw.txt": "pw\n",
    "base.yaml": "services:\n  base:\n    image: nginx:alpine\n    environment: [BASE=1]\n",
}
NOISE = ("Network ", "Volume ", "Container ", "Image ", "nginx:alpine: not pulled")


def compose(cwd, project, *args, timeout=150):
    try:
        return subprocess.run([*COMPOSE, "-p", project, *args], cwd=cwd, env=ENV,
                              capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return subprocess.CompletedProcess(args, -1, "", f"timed out after {timeout}s")


def run(c):
    kind, want = c["expect"]
    if kind == "known":
        return "KNOWN", c["note"]
    project = "cs-" + c["attr"].replace(".", "-").replace("_", "-")[:40].strip("-")
    with tempfile.TemporaryDirectory() as d:
        for name, content in FILES.items():
            open(os.path.join(d, name), "w").write(content)
        os.mkdir(os.path.join(d, "src"))
        open(os.path.join(d, "compose.yaml"), "w").write(
            "services:\n  web:\n    image: nginx:alpine\n    " + c["service"] + "\n" + c["extra"] + c["top"] + "\n")
        up = compose(d, project, "up", "-d")
        out = [l.strip() for l in (up.stdout + up.stderr).splitlines()
               if l.strip() and not l.strip().startswith(NOISE)]
        got = subprocess.run(["kubectl", "--context", CONTEXT, "-n", "d2k", "get", "deploy", "-o", "json",
                              "-l", f"com.docker.compose.project={project},com.docker.compose.service=web"],
                             capture_output=True, text=True)
        deps = json.loads(got.stdout or '{"items": []}')["items"]
        compose(d, project, "down", "-v", "-t", "1")

    text = "\n".join(out)
    if kind == "error":
        return ("PASS", "") if up.returncode != 0 and want in text else ("FAIL", f"want error {want!r}: rc={up.returncode} {text}")
    if up.returncode != 0:
        return "FAIL", f"rc={up.returncode}: {text}"
    if kind == "warn":
        return ("PASS", "") if want in text else ("FAIL", f"want warning {want!r}, got: {text or 'nothing'}")
    if out:
        return "FAIL", f"unexpected output: {text}"
    if kind == "absent":
        return ("PASS", "") if not deps else ("FAIL", "service created")
    if kind == "count":
        return ("PASS", "") if len(deps) == want else ("FAIL", f"{len(deps)} containers, want {want}")
    if not deps:
        return "FAIL", "no container"
    try:
        return ("PASS", "") if want is None or want(deps) else ("FAIL", "Kubernetes result does not match")
    except (KeyError, IndexError, TypeError) as e:
        return "FAIL", f"Kubernetes result does not match: missing {e}"


def write_doc():
    lines = [
        "# Compose Specification support",
        "",
        "<!-- Generated by test/e2e/compose_spec.py --doc. Edit CASES there, not this file. -->",
        "",
        "d2k runs the real `docker compose` CLI (and Portainer's compose library) unchanged: compose reads the",
        "compose file on the client and sends d2k Docker Engine API calls, which d2k translates to Kubernetes.",
        "Every attribute of the [Compose Specification](https://github.com/compose-spec/compose-spec) is",
        "listed here, and `test/e2e/compose_spec.py` checks each row against a cluster: an attribute either",
        "works, or `docker compose up` warns or fails. None is dropped silently.",
        "",
        f"- **{YES}**: translated to Kubernetes.",
        f"- **{PARTIAL}**: translated, with the limits noted; the rest warns.",
        f"- **{NO}**: `docker compose up` warns (the setting is ignored) or fails.",
        f"- **{CLIENT}**: handled by compose on the client; d2k sees its effect.",
        "",
    ]
    groups = [("Services", lambda a: ".top." not in a), ("Top-level elements", lambda a: ".top." in a)]
    for title, member in groups:
        lines += [f"## {title}", "", "| Attribute | Support | Notes |", "|---|---|---|"]
        for c in CASES:
            if member(c["attr"]):
                lines.append(f"| `{c['attr'].replace('.top.', '.')}` | {c['support']} | {c['note']} |")
        lines.append("")
    path = os.path.join(ROOT, "docs", "compose-support.md")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    open(path, "w").write("\n".join(lines))
    print("wrote", os.path.relpath(path, ROOT))


def main(args):
    if args == ["--doc"]:
        write_doc()
        return 0
    cases = [c for c in CASES if c["expect"] is not None and (not args or c["attr"] in args)]
    with ThreadPoolExecutor(max_workers=6) as pool:
        results = list(pool.map(run, cases))
    failed = 0
    for c, (status, detail) in zip(cases, results):
        failed += status == "FAIL"
        print(f"{status:6} {c['attr']}" + (f"\n         {detail}" if detail and status == "FAIL" else ""))
    counts = {s: sum(r[0] == s for r in results) for s in ("PASS", "FAIL", "KNOWN")}
    print(f"\npassed: {counts['PASS']}  failed: {counts['FAIL']}  known gaps: {counts['KNOWN']}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
