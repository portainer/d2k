package adapter

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/rs/zerolog/log"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"

	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/internal/types"
	"github.com/portainer/d2k/pkg/portmapper"
)

// RunOptions mirrors the subset of docker run flags that d2k supports.
type RunOptions struct {
	Name         string
	Image        string
	Entrypoint   []string
	Cmd          []string
	Env          []string
	Labels       map[string]string
	PortBindings []string
	PublishAll   bool
	ExposedPorts map[string]struct{}
	Volumes      []string
	GPUCount     int
	// Binds are HostConfig.Binds entries: "<volume>:<path>[:<options>]".
	Binds []string
	// Aliases are the container's network aliases (NetworkingConfig).
	Aliases []string
	// Networks are the names of the Docker networks the container joins.
	Networks []string

	WorkingDir  string
	Tty         bool
	Stdin       bool
	StdinOnce   bool
	RunAsUser   *int64
	RunAsGroup  *int64
	StopTimeout *int64 // seconds; the pod's termination grace period
	HostAliases []corev1.HostAlias
	Resources   corev1.ResourceRequirements
	CapAdd      []string
	CapDrop     []string
	Privileged  bool
	ReadOnly    bool
	Healthcheck *corev1.Probe // readiness probe; the container's health
	// RestartPolicy is Docker's: "no" and "on-failure" containers run as
	// pods, "always" and "unless-stopped" ones as Deployment replicas.
	RestartPolicy string
	// AutoRemove removes the container when its run ends.
	AutoRemove bool

	// Hostname is the pod's hostname. Compose services keep their service
	// name, which their DNS needs.
	Hostname   string
	DNSSearch  []string
	DNSOptions []corev1.PodDNSConfigOption
	Sysctls    []corev1.Sysctl
	// Tmpfs are memory-backed mounts, including /dev/shm for ShmSize.
	Tmpfs              []TmpfsMount
	NoNewPrivileges    bool
	Seccomp            *corev1.SeccompProfile
	AppArmor           *corev1.AppArmorProfile
	SELinux            *corev1.SELinuxOptions
	SupplementalGroups []int64
	// Annotations are set on the pod.
	Annotations map[string]string
	// VolumeSubpaths maps mount targets to the volume subdirectory mounted.
	VolumeSubpaths map[string]string
}

// TmpfsMount is a memory-backed mount.
type TmpfsMount struct {
	Target    string
	SizeLimit *resource.Quantity
}

// ContainerSummary is a Docker-compatible summary row, as returned by docker ps.
type ContainerSummary struct {
	ID        string
	Names     []string
	Image     string
	Status    string
	State     string
	Created   int64
	Ports     []container.PortSummary
	Labels    map[string]string
	IPAddress string

	// Networks maps the names of the container's networks to their IDs.
	Networks map[string]string
	Mounts   []container.MountPoint
}

// CreateContainer implements docker run: creates a Deployment and, where
// required, a Service in the target namespace.
func (a *KubernetesDockerAdapter) CreateContainer(ctx context.Context, opts RunOptions) (string, []string, error) {
	if opts.Name == "" {
		return "", nil, errdefs.InvalidParameter(fmt.Errorf("container name is required"))
	}
	if opts.AutoRemove && opts.RestartPolicy != "" && opts.RestartPolicy != string(container.RestartPolicyDisabled) {
		return "", nil, errdefs.InvalidParameter(fmt.Errorf("can't create 'AutoRemove' container with restart policy"))
	}

	dockerName := strings.TrimPrefix(opts.Name, "/")
	inUse, err := a.dockerNameInUse(ctx, dockerName)
	if err != nil {
		return "", nil, err
	}
	if inUse {
		return "", nil, errdefs.Conflict(fmt.Errorf("container name %q is already in use", dockerName))
	}

	// Sanitise the name for Kubernetes: lowercase, underscores to hyphens, max 63 chars.
	opts.Name = strings.ToLower(strings.ReplaceAll(opts.Name, "_", "-"))
	if len(opts.Name) > 63 {
		opts.Name = opts.Name[:63]
	}
	opts.Name = strings.TrimRight(opts.Name, "-")

	// A renamed container keeps its Deployment name, which may be the name
	// requested here. The Docker name is free, so use another Deployment name.
	if _, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, opts.Name, metav1GetOptions()); err == nil {
		opts.Name = strings.TrimRight(truncate(opts.Name, 57), "-") + "-" + rand.String(5)
	}

	// Resolve port mappings and determine Service type.
	pmReq := portmapper.Request{
		PublishAll:   opts.PublishAll,
		ExposedPorts: opts.ExposedPorts,
		PortBindings: opts.PortBindings,
	}

	kind, mappings, warnings, err := portmapper.Resolve(pmReq)
	if err != nil {
		return "", nil, fmt.Errorf("unable to resolve port mappings: %w", err)
	}

	volumes, volumeMounts, volumeNames, volumeWarnings, err := a.namedVolumeMounts(ctx, opts.Binds)
	if err != nil {
		return "", nil, err
	}
	warnings = append(warnings, volumeWarnings...)
	warnings = append(warnings, a.networkWarnings(ctx, opts.Networks)...)
	for i := range volumeMounts {
		volumeMounts[i].SubPath = opts.VolumeSubpaths[volumeMounts[i].MountPath]
	}
	for i, m := range opts.Tmpfs {
		name := fmt.Sprintf("tmpfs-%d", i)
		volumes = append(volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: m.SizeLimit},
		}})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: name, MountPath: m.Target})
	}
	if opts.Hostname != "" && len(validation.IsDNS1123Label(opts.Hostname)) > 0 {
		warnings = append(warnings, fmt.Sprintf("hostname %q ignored: a pod hostname must be a DNS label", opts.Hostname))
		opts.Hostname = ""
	}
	if opts.GPUCount > 0 && a.gpuResourceName == "" {
		warnings = append(warnings, "GPU request ignored: set D2K_GPU_RESOURCE_NAME to the device plugin resource name")
	}

	// Build and create the Deployment.
	deployment, err := a.buildDeployment(ctx, opts, kind, mappings)
	if err != nil {
		return "", nil, fmt.Errorf("unable to build deployment: %w", err)
	}
	deployment.Annotations[types.AnnotationDockerName] = dockerName
	if opts.RestartPolicy != "" {
		deployment.Annotations[types.AnnotationRestartPolicy] = opts.RestartPolicy
	}
	if opts.AutoRemove {
		deployment.Annotations[types.AnnotationAutoRemove] = "true"
	}
	if len(opts.Labels) > 0 {
		dockerLabels, err := json.Marshal(opts.Labels)
		if err != nil {
			return "", nil, fmt.Errorf("unable to encode labels: %w", err)
		}
		deployment.Annotations[types.AnnotationDockerLabels] = string(dockerLabels)
	}
	if len(volumeNames) > 0 {
		raw, err := json.Marshal(volumeNames)
		if err != nil {
			return "", nil, fmt.Errorf("unable to encode volume names: %w", err)
		}
		deployment.Annotations[types.AnnotationVolumeNames] = string(raw)
	}
	deployment.Spec.Template.Spec.Volumes = volumes
	deployment.Spec.Template.Spec.Containers[0].VolumeMounts = volumeMounts

	if len(opts.Networks) > 0 {
		networks, err := json.Marshal(opts.Networks)
		if err != nil {
			return "", nil, fmt.Errorf("unable to encode networks: %w", err)
		}
		deployment.Annotations[types.AnnotationNetworks] = string(networks)
	}

	// Compose makes each service resolvable by its service name.
	project := opts.Labels["com.docker.compose.project"]
	service := opts.Labels["com.docker.compose.service"]
	scoped := project != "" && service != ""
	if scoped {
		if opts.Hostname != "" {
			warnings = append(warnings, fmt.Sprintf("hostname %q ignored: compose services use their service name, which their DNS needs", opts.Hostname))
		}
		warnings = append(warnings, a.scopeDNSToProject(deployment, project, service)...)
	}
	warnings = append(warnings, ignoredAliases(opts.Aliases, dockerName, service)...)

	// From here the container is created whole or rolled back, even if the
	// client goes away: a half-created one would hold its name.
	ctx = context.WithoutCancel(ctx)
	created, err := a.client.AppsV1().Deployments(a.namespace).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			return "", nil, errdefs.Conflict(fmt.Errorf("container name %q is already in use", opts.Name))
		}
		if errors.IsForbidden(err) {
			return "", nil, fmt.Errorf("deployment rejected by Kubernetes: %w", err)
		}
		return "", nil, fmt.Errorf("unable to create deployment %q: %w", opts.Name, err)
	}

	// Always create a ClusterIP Service so the container name resolves via
	// Kubernetes DNS from other pods in the namespace. This makes Docker
	// short-name DNS work (e.g. "redis", "postgres") without needing FQDNs.
	// Build ClusterIP ports from container port mappings.
	var clusterPorts []corev1.ServicePort
	for i, m := range mappings {
		clusterPorts = append(clusterPorts, corev1.ServicePort{
			Name:       fmt.Sprintf("port-%d", i),
			Protocol:   corev1.Protocol(m.Protocol),
			Port:       int32(m.ContainerPort),
			TargetPort: intstr.FromInt(m.ContainerPort),
		})
	}
	// Use headless (clusterIP: None) when there are no ports — Kubernetes rejects
	// ClusterIP Services with an empty ports list but headless Services are allowed
	// without ports and still register the DNS name for short-name resolution.
	clusterSpec := corev1.ServiceSpec{
		Selector: map[string]string{"app": opts.Name},
		Ports:    clusterPorts,
		// A failing healthcheck must not take the container out of service.
		PublishNotReadyAddresses: true,
	}
	if len(clusterPorts) == 0 {
		clusterSpec.ClusterIP = "None"
	} else {
		clusterSpec.Type = corev1.ServiceTypeClusterIP
	}
	clusterSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName(opts.Name),
			Namespace: a.namespace,
			Labels:    managedLabels(opts.Name),
			Annotations: map[string]string{
				"d2k.portainer.io/dns-service": "true",
			},
		},
		Spec: clusterSpec,
	}
	_, svcErr := a.client.CoreV1().Services(a.namespace).Create(ctx, clusterSvc, metav1.CreateOptions{})
	if errors.IsAlreadyExists(svcErr) && a.ownService(ctx, clusterSvc.Name, opts.Name) == nil {
		svcErr = errdefs.Conflict(fmt.Errorf("the name %q is taken by Service %q, which is not this container's", dockerName, clusterSvc.Name))
	}
	if svcErr != nil && !errors.IsAlreadyExists(svcErr) {
		_ = a.client.AppsV1().Deployments(a.namespace).Delete(ctx, opts.Name, metav1.DeleteOptions{})
		return "", nil, fmt.Errorf("unable to create ClusterIP service for %q: %w", opts.Name, svcErr)
	}

	// Create a LoadBalancer or NodePort Service for externally published ports.
	if kind != portmapper.NoService && len(mappings) > 0 {
		svc, svcErr := a.buildService(opts.Name, kind, mappings)
		if svcErr != nil {
			return "", nil, fmt.Errorf("unable to build service: %w", svcErr)
		}
		if _, svcErr = a.client.CoreV1().Services(a.namespace).Create(ctx, svc, metav1.CreateOptions{}); svcErr != nil {
			// Roll back the Deployment and ClusterIP service so we don't leave orphans.
			_ = a.client.AppsV1().Deployments(a.namespace).Delete(ctx, opts.Name, metav1.DeleteOptions{})
			a.deleteOwnService(ctx, serviceName(opts.Name), opts.Name)
			return "", nil, fmt.Errorf("unable to create service for %q: %w", opts.Name, svcErr)
		}
	}

	if scoped {
		if err := a.ensureProjectDNSService(ctx, project, created); err != nil {
			_ = a.client.AppsV1().Deployments(a.namespace).Delete(ctx, opts.Name, metav1.DeleteOptions{})
			a.deleteOwnService(ctx, serviceName(opts.Name), opts.Name)
			a.deleteOwnService(ctx, publishedServiceName(opts.Name), opts.Name)
			return "", nil, err
		}
	}

	return containerID(created.UID), warnings, nil
}

// ListContainers implements docker ps.
func (a *KubernetesDockerAdapter) ListContainers(ctx context.Context, all bool) ([]ContainerSummary, error) {
	deployments, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return nil, fmt.Errorf("unable to list deployments: %w", err)
	}

	pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return nil, fmt.Errorf("unable to list pods: %w", err)
	}
	podsByContainer := map[string][]corev1.Pod{}
	for _, p := range pods.Items {
		podsByContainer[p.Labels["app"]] = append(podsByContainer[p.Labels["app"]], p)
	}
	// One list for all containers: a lookup per container is throttled by
	// the client's rate limit and made `docker ps` take seconds.
	services, err := a.client.CoreV1().Services(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return nil, fmt.Errorf("unable to list services: %w", err)
	}
	servicesByName := map[string]corev1.Service{}
	for _, svc := range services.Items {
		servicesByName[svc.Name] = svc
	}

	var summaries []ContainerSummary

	for _, d := range deployments.Items {
		summary := deploymentToSummary(d, podsByContainer[d.Name])
		if !all && summary.State != string(container.StateRunning) && summary.State != string(container.StateRestarting) {
			continue
		}

		// The published Service, if any, gives the LoadBalancer IP.
		if svc, ok := servicesByName[publishedServiceName(d.Name)]; ok && len(svc.Status.LoadBalancer.Ingress) > 0 {
			summary.IPAddress = svc.Status.LoadBalancer.Ingress[0].IP
			if summary.IPAddress == "" {
				summary.IPAddress = svc.Status.LoadBalancer.Ingress[0].Hostname
			}
		}

		summaries = append(summaries, summary)
	}

	return summaries, nil
}

// StopContainer implements docker stop: scales the Deployment to 0 replicas.
// Returns an error if the Deployment is managed by the Swarm layer.
func (a *KubernetesDockerAdapter) StopContainer(ctx context.Context, name string) error {
	resolved, err := a.resolveDeploymentName(ctx, name)
	if err != nil {
		return err
	}
	d, getErr := a.client.AppsV1().Deployments(a.namespace).Get(ctx, resolved, metav1GetOptions())
	if getErr == nil && d.Labels[types.LabelSwarmManagedBy] == types.LabelSwarmManagedByValue {
		return errdefs.Conflict(fmt.Errorf("cannot stop a container that is managed by swarm: use docker service scale instead"))
	}
	if getErr == nil && runsAsPod(*d) {
		return a.stopPod(ctx, d)
	}
	if err := a.scaleDeployment(ctx, name, 0); err != nil {
		return err
	}
	// As Docker's stop does, return once the container has stopped: compose
	// starts its replacement, on the same volumes, right after.
	grace := int64(corev1.DefaultTerminationGracePeriodSeconds)
	if getErr == nil && d.Spec.Template.Spec.TerminationGracePeriodSeconds != nil {
		grace = *d.Spec.Template.Spec.TerminationGracePeriodSeconds
	}
	if code, ok := a.waitForPodsGone(ctx, resolved, time.Duration(grace+30)*time.Second); ok {
		// A container removed meanwhile has no state left to record.
		if err := a.annotate(ctx, resolved, map[string]*string{types.AnnotationExitCode: &code}); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// StartContainer implements docker start: it runs a pod for a container that
// runs once, and scales the Deployment back to 1 replica otherwise.
func (a *KubernetesDockerAdapter) StartContainer(ctx context.Context, name string) error {
	resolved, err := a.resolveDeploymentName(ctx, name)
	if err != nil {
		return err
	}
	d, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, resolved, metav1GetOptions())
	if err != nil {
		return fmt.Errorf("unable to get deployment %q: %w", resolved, err)
	}
	if runsAsPod(*d) {
		err := a.startPod(ctx, d)
		if err != nil && d.Annotations[types.AnnotationAutoRemove] == "true" {
			a.removeFailedStart(ctx, resolved)
		}
		return err
	}
	if err := a.scaleDeployment(ctx, name, 1); err != nil {
		return err
	}
	if err := a.waitUntilPodCreated(ctx, resolved, podCreateTimeout); err != nil {
		if d.Annotations[types.AnnotationAutoRemove] == "true" {
			a.removeFailedStart(ctx, resolved)
			return err
		}
		// As Docker's failed start does, leave the container stopped: at 1
		// replica it would start by itself once the quota frees.
		if scaleErr := a.scaleDeployment(context.WithoutCancel(ctx), resolved, 0); scaleErr != nil {
			log.Warn().Str("context", "KubernetesAdapter").Str("container", resolved).Err(scaleErr).Msg("Unable to stop a container whose start failed")
		}
		return err
	}
	return nil
}

// removeFailedStart removes an AutoRemove container whose start failed, as
// Docker does, the same as one whose run ends: docker run --rm waits for
// the removal before it reports the start's error.
func (a *KubernetesDockerAdapter) removeFailedStart(ctx context.Context, name string) {
	if err := a.RemoveContainer(context.WithoutCancel(ctx), name); err != nil {
		log.Warn().Str("context", "KubernetesAdapter").Str("container", name).Err(err).Msg("Unable to remove an AutoRemove container whose start failed")
	}
}

// RestartContainer implements docker restart: it stops the container, then
// starts it. As in Docker, the end of the run a restart stops does not
// remove an AutoRemove container.
func (a *KubernetesDockerAdapter) RestartContainer(ctx context.Context, name string) error {
	// As in Docker, a client that goes away does not cancel the restart: a
	// stop without its start would leave the container stopped (D2K-083).
	ctx = context.WithoutCancel(ctx)
	resolved, err := a.resolveDeploymentName(ctx, name)
	if err != nil {
		return err
	}
	// Recorded on the container, not in d2k: another d2k, as in a rollout,
	// sees it, and finishes the restart if this one stops before the start.
	if err := a.annotate(ctx, resolved, map[string]*string{types.AnnotationRestarting: ptr(time.Now().UTC().Format(time.RFC3339Nano))}); err != nil {
		return fmt.Errorf("unable to restart container %q: %w", name, err)
	}
	defer a.clearRestarting(ctx, resolved)
	if err := a.StopContainer(ctx, name); err != nil {
		return err
	}
	if err := a.StartContainer(ctx, name); err != nil {
		return err
	}
	// As Docker's restart does, return once the container runs again:
	// clients that list containers right after, as compose pause and port
	// do, skip one that is still starting.
	a.waitUntilRunStarted(ctx, resolved, restartStartTimeout)
	return nil
}

// RemoveContainer implements docker rm: deletes the Deployment and its associated Service (if any).
// Returns an error if the Deployment is managed by the Swarm layer — use docker service rm instead.
func (a *KubernetesDockerAdapter) RemoveContainer(ctx context.Context, name string) error {
	resolved, err := a.resolveDeploymentName(ctx, name)
	if err != nil {
		return err
	}

	// Refuse to remove containers that back a swarm service — the same guard
	// Docker Swarm applies: "cannot remove a running container that is managed by swarm".
	d, getErr := a.client.AppsV1().Deployments(a.namespace).Get(ctx, resolved, metav1GetOptions())
	if getErr == nil && d.Labels[types.LabelSwarmManagedBy] == types.LabelSwarmManagedByValue {
		return errdefs.Conflict(fmt.Errorf("cannot remove a running container that is managed by swarm: use docker service rm instead"))
	}
	if getErr == nil && runsAsPod(*d) {
		a.killRunOncePods(ctx, resolved)
	}

	if err := a.client.AppsV1().Deployments(a.namespace).Delete(ctx, resolved, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("unable to delete deployment %q: %w", resolved, err)
	}
	a.killPods(ctx, resolved)

	// Best-effort Service deletion — remove ClusterIP DNS service and LB/NodePort service.
	a.deleteOwnService(ctx, serviceName(resolved), resolved)
	a.deleteOwnService(ctx, publishedServiceName(resolved), resolved)

	return nil
}

// killRunOncePods deletes a run-once container's pods with a 1 s grace
// period before its Deployment goes. The kubelet stops a pod with the grace
// period of the first delete it sees, and the garbage collector's would be
// the full stop grace period. Nothing replaces these pods.
func (a *KubernetesDockerAdapter) killRunOncePods(ctx context.Context, name string) {
	grace := int64(1)
	pods := a.client.CoreV1().Pods(a.namespace)
	list, err := pods.List(ctx, podsOf(name))
	if err != nil {
		return
	}
	for _, p := range list.Items {
		if p.DeletionTimestamp == nil {
			_ = pods.Delete(ctx, p.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace})
		}
	}
}

// killPods cuts the grace period of a removed container's pods to 1 s, as
// Docker's rm -f kills the container, and waits until they are gone: the
// garbage collector deletes them with their stop grace period. A kubelet
// that has already begun stopping a pod keeps the longer one.
func (a *KubernetesDockerAdapter) killPods(ctx context.Context, name string) {
	// Not 0: that removes the pod from the API before the kubelet has killed
	// its container, so its volumes would be released while it still runs.
	grace := int64(1)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(corev1.DefaultTerminationGracePeriodSeconds+30)*time.Second)
	defer cancel()
	pods := a.client.CoreV1().Pods(a.namespace)
	for {
		list, err := pods.List(ctx, podsOf(name))
		if err != nil || len(list.Items) == 0 {
			return
		}
		for _, p := range list.Items {
			// Only a pod the collector is deleting: its ReplicaSet, which d2k
			// may not touch, would replace one deleted before.
			if g := p.DeletionGracePeriodSeconds; p.DeletionTimestamp != nil && (g == nil || *g > grace) {
				_ = pods.Delete(ctx, p.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace})
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// RenameContainer implements docker rename. The Docker name lives in an
// annotation, so the Deployment, and with it the container ID, is unchanged.
func (a *KubernetesDockerAdapter) RenameContainer(ctx context.Context, nameOrID, newName string) error {
	resolved, err := a.resolveDeploymentName(ctx, nameOrID)
	if err != nil {
		return err
	}

	newName = strings.TrimPrefix(newName, "/")
	inUse, err := a.dockerNameInUse(ctx, newName)
	if err != nil {
		return err
	}
	if inUse {
		return errdefs.Conflict(fmt.Errorf("container name %q is already in use", newName))
	}

	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{types.AnnotationDockerName: newName}},
	})
	if err != nil {
		return err
	}
	if _, err := a.client.AppsV1().Deployments(a.namespace).Patch(ctx, resolved, k8stypes.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("unable to rename container %q: %w", nameOrID, err)
	}
	return nil
}

// InspectContainer implements docker inspect for a single container.
func (a *KubernetesDockerAdapter) InspectContainer(ctx context.Context, name string) (*container.InspectResponse, error) {
	resolved, err := a.resolveDeploymentName(ctx, name)
	if err != nil {
		return nil, err
	}

	d, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, resolved, metav1GetOptions())
	if err != nil {
		return nil, fmt.Errorf("unable to get deployment %q: %w", resolved, err)
	}

	lbIP := ""
	svc, svcErr := a.client.CoreV1().Services(a.namespace).Get(ctx, publishedServiceName(resolved), metav1GetOptions())
	if svcErr == nil && len(svc.Status.LoadBalancer.Ingress) > 0 {
		lbIP = svc.Status.LoadBalancer.Ingress[0].IP
		if lbIP == "" {
			lbIP = svc.Status.LoadBalancer.Ingress[0].Hostname
		}
	}

	pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, podsOf(d.Name))
	if err != nil {
		return nil, fmt.Errorf("unable to list pods of %q: %w", resolved, err)
	}

	result := deploymentToContainerJSON(*d, pods.Items, lbIP)
	if c := d.Spec.Template.Spec.Containers; a.gpuResourceName != "" && len(c) > 0 {
		if q, ok := c[0].Resources.Limits[corev1.ResourceName(a.gpuResourceName)]; ok && q.Value() > 0 {
			// The request docker run --gpus N sends.
			result.HostConfig.DeviceRequests = []container.DeviceRequest{{Count: int(q.Value()), Capabilities: [][]string{{"gpu"}}}}
		}
	}
	// A restart stops the container fully before it starts it again. Docker
	// starts it again at once, so clients that inspect it on the restart's
	// die, as compose does, see it running, not exited.
	if _, restarting := d.Annotations[types.AnnotationRestarting]; restarting && result.State.Status == container.StateExited {
		result.State.Status, result.State.Running, result.State.Restarting = container.StateRestarting, true, true
	}
	return &result, nil
}

// --- builders ---

func (a *KubernetesDockerAdapter) buildDeployment(ctx context.Context, opts RunOptions, kind portmapper.MappingKind, mappings []portmapper.PortMapping) (*appsv1.Deployment, error) {
	labels := managedLabels(opts.Name)
	addDockerLabels(labels, opts.Labels)

	portAnnotation, err := encodePortMappings(opts.PortBindings, opts.PublishAll)
	if err != nil {
		return nil, err
	}
	annotations := map[string]string{
		types.AnnotationPortMappings: portAnnotation,
		types.AnnotationImageRef:     opts.Image,
	}

	serviceTypeLabel := types.ServiceTypeNone
	switch kind {
	case portmapper.LoadBalancerService:
		serviceTypeLabel = types.ServiceTypeLB
	case portmapper.NodePortService:
		serviceTypeLabel = types.ServiceTypeNodePort
	}
	labels[types.LabelServiceType] = serviceTypeLabel

	var containerPorts []corev1.ContainerPort
	for _, m := range mappings {
		containerPorts = append(containerPorts, corev1.ContainerPort{
			ContainerPort: int32(m.ContainerPort),
			Protocol:      corev1.Protocol(m.Protocol),
		})
	}

	var envVars []corev1.EnvVar
	for _, e := range opts.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) != 2 {
			continue
		}
		envVars = append(envVars, corev1.EnvVar{Name: parts[0], Value: parts[1]})
	}

	requested := *opts.Resources.DeepCopy()
	if opts.GPUCount > 0 && a.gpuResourceName != "" {
		if requested.Limits == nil {
			requested.Limits = corev1.ResourceList{}
		}
		requested.Limits[corev1.ResourceName(a.gpuResourceName)] = resource.MustParse(fmt.Sprintf("%d", opts.GPUCount))
	}

	// Inject requests.cpu / requests.memory when the namespace has a ResourceQuota
	// that requires them. Docker has no concept of ResourceQuotas so the caller
	// cannot know they are needed. injectQuotaDefaults is a no-op when no quota exists.
	resourceReqs, err := a.injectQuotaDefaults(ctx, requested)
	if err != nil {
		return nil, err
	}

	// Created, not started: start scales the Deployment up.
	replicas := int32(0)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        opts.Name,
			Namespace:   a.namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": opts.Name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":                   opts.Name,
						types.LabelManagedBy:    types.LabelManagedByValue,
						types.LabelWorkloadName: opts.Name,
					},
					Annotations: opts.Annotations,
				},
				Spec: corev1.PodSpec{
					// Docker sets no <SERVICE>_PORT variables; they collide
					// with apps' own (MEMOS_PORT for a container named memos).
					EnableServiceLinks:            new(bool),
					Hostname:                      opts.Hostname,
					DNSConfig:                     dnsConfig(opts),
					SecurityContext:               podSecurityContext(opts),
					HostAliases:                   opts.HostAliases,
					TerminationGracePeriodSeconds: opts.StopTimeout,
					Containers: []corev1.Container{
						{
							Name:            opts.Name,
							Image:           opts.Image,
							Command:         opts.Entrypoint,
							Args:            opts.Cmd,
							Env:             envVars,
							Ports:           containerPorts,
							WorkingDir:      opts.WorkingDir,
							TTY:             opts.Tty,
							Stdin:           opts.Stdin,
							StdinOnce:       opts.StdinOnce,
							SecurityContext: containerSecurityContext(opts),
							ReadinessProbe:  opts.Healthcheck,
							Resources:       resourceReqs,
						},
					},
				},
			},
		},
	}, nil
}

func (a *KubernetesDockerAdapter) buildService(name string, kind portmapper.MappingKind, mappings []portmapper.PortMapping) (*corev1.Service, error) {
	var svcType corev1.ServiceType
	switch kind {
	case portmapper.LoadBalancerService:
		svcType = corev1.ServiceTypeLoadBalancer
	case portmapper.NodePortService:
		svcType = corev1.ServiceTypeNodePort
	default:
		return nil, fmt.Errorf("buildService called with kind %d, which requires no service", kind)
	}

	var ports []corev1.ServicePort
	for i, m := range mappings {
		sp := corev1.ServicePort{
			Name:       fmt.Sprintf("port-%d", i),
			Port:       int32(m.HostPort),
			TargetPort: intstr.FromInt(m.ContainerPort),
			Protocol:   corev1.Protocol(m.Protocol),
		}
		if m.HostPort == 0 {
			sp.Port = int32(m.ContainerPort)
		}
		ports = append(ports, sp)
	}

	svcName := publishedServiceName(name)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: a.namespace,
			Labels:    managedLabels(name),
		},
		Spec: corev1.ServiceSpec{
			Type:                     svcType,
			Selector:                 map[string]string{"app": name},
			Ports:                    ports,
			PublishNotReadyAddresses: true,
		},
	}, nil
}

// serviceName returns the Kubernetes Service name for a given deployment name.
// Service names must conform to DNS-1035: start with a letter. If the deployment
// name starts with a digit, we prefix with "svc-".
func serviceName(name string) string {
	if len(name) > 0 && name[0] >= '0' && name[0] <= '9' {
		name = "svc-" + name
	}
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

// ownService returns the named Service if d2k created it for the container
// whose Deployment is named deployment, and nil otherwise: another
// container's published Service, or another workload's Service, may have
// its name.
func (a *KubernetesDockerAdapter) ownService(ctx context.Context, name, deployment string) *corev1.Service {
	svc, err := a.client.CoreV1().Services(a.namespace).Get(ctx, name, metav1GetOptions())
	if err != nil || svc.Labels[types.LabelManagedBy] != types.LabelManagedByValue || svc.Labels[types.LabelWorkloadName] != deployment {
		return nil
	}
	return svc
}

// deleteOwnService deletes the named Service, best effort, if it is the
// container's.
func (a *KubernetesDockerAdapter) deleteOwnService(ctx context.Context, name, deployment string) {
	if svc := a.ownService(ctx, name, deployment); svc != nil {
		_ = a.client.CoreV1().Services(a.namespace).Delete(ctx, name, metav1.DeleteOptions{Preconditions: metav1.NewUIDPreconditions(string(svc.UID))})
	}
}

// --- name/ID resolution ---

func (a *KubernetesDockerAdapter) resolveDeploymentName(ctx context.Context, nameOrID string) (string, error) {
	nameOrID = strings.TrimPrefix(nameOrID, "/")
	d, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, nameOrID, metav1GetOptions())
	if err == nil && d.Labels[types.LabelManagedBy] == types.LabelManagedByValue && dockerName(*d) == nameOrID {
		return nameOrID, nil
	}
	if err != nil && !errors.IsNotFound(err) {
		return "", fmt.Errorf("unable to look up container %q: %w", nameOrID, err)
	}
	if nameOrID == "" {
		return "", errdefs.NotFound(fmt.Errorf("container name or ID is empty"))
	}
	list, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return "", fmt.Errorf("unable to list deployments: %w", err)
	}
	// As in Docker: a full ID or an exact name, else a unique ID prefix.
	var byPrefix []string
	for _, d := range list.Items {
		id := containerID(d.UID)
		if id == nameOrID || dockerName(d) == nameOrID {
			return d.Name, nil
		}
		if strings.HasPrefix(id, nameOrID) {
			byPrefix = append(byPrefix, d.Name)
		}
	}
	switch len(byPrefix) {
	case 1:
		return byPrefix[0], nil
	case 0:
	default:
		return "", errdefs.InvalidParameter(fmt.Errorf("multiple IDs found with provided prefix: %s", nameOrID))
	}
	return "", errdefs.NotFound(fmt.Errorf("container %q not found", nameOrID))
}

// --- scale helper ---

func (a *KubernetesDockerAdapter) scaleDeployment(ctx context.Context, name string, replicas int32) error {
	resolved, err := a.resolveDeploymentName(ctx, name)
	if err != nil {
		return err
	}

	for attempt := 0; attempt < 5; attempt++ {
		d, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, resolved, metav1GetOptions())
		if err != nil {
			return fmt.Errorf("unable to get deployment %q: %w", resolved, err)
		}

		d.Spec.Replicas = &replicas
		if d.Annotations == nil {
			d.Annotations = map[string]string{}
		}
		stamp := types.AnnotationStoppedAt
		if replicas > 0 {
			stamp = types.AnnotationStartedAt
			delete(d.Annotations, types.AnnotationExitCode)
		}
		d.Annotations[stamp] = time.Now().UTC().Format(time.RFC3339Nano)
		_, err = a.client.AppsV1().Deployments(a.namespace).Update(ctx, d, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if !errors.IsConflict(err) {
			return fmt.Errorf("unable to scale deployment %q to %d: %w", name, replicas, err)
		}
	}

	return fmt.Errorf("unable to scale deployment %q to %d: too many conflicts", name, replicas)
}

// --- converters ---

func deploymentToSummary(d appsv1.Deployment, pods []corev1.Pod) ContainerSummary {
	now := time.Now()
	s := containerState(d, pods, now)
	state, status := string(s.Status), containerStatus(s, now)

	var ports []container.PortSummary
	for _, m := range publishedPorts(d) {
		ports = append(ports, container.PortSummary{
			IP:          netip.IPv4Unspecified(),
			PrivatePort: uint16(m.ContainerPort),
			PublicPort:  uint16(m.HostPort),
			Type:        strings.ToLower(m.Protocol),
		})
	}

	return ContainerSummary{
		ID:      containerID(d.UID),
		Names:   []string{"/" + dockerName(d)},
		Image:   d.Annotations[types.AnnotationImageRef],
		Status:  status,
		State:   state,
		Created: d.CreationTimestamp.Unix(),
		Labels:  dockerLabels(d),
		Ports:   ports,

		Networks: networkIDs(d),
		Mounts:   containerMounts(d),
	}
}

// publishedPorts returns the container's explicit port bindings, as
// recorded when it was created.
func publishedPorts(d appsv1.Deployment) []portmapper.PortMapping {
	var bindings []string
	if raw := d.Annotations[types.AnnotationPortMappings]; raw == "" || json.Unmarshal([]byte(raw), &bindings) != nil {
		return nil
	}
	var mappings []portmapper.PortMapping
	for _, b := range bindings {
		if _, m, _, err := portmapper.Resolve(portmapper.Request{PortBindings: []string{b}}); err == nil {
			mappings = append(mappings, m...)
		}
	}
	return mappings
}

// networkIDs maps the names of a container's networks to their IDs.
func networkIDs(d appsv1.Deployment) map[string]string {
	ids := map[string]string{}
	for _, name := range containerNetworks(d) {
		ids[name] = networkIDForName(name, d.Namespace)
	}
	return ids
}

func deploymentToContainerJSON(d appsv1.Deployment, pods []corev1.Pod, lbIP string) container.InspectResponse {
	state := containerState(d, pods, time.Now())

	var spec corev1.Container
	if containers := d.Spec.Template.Spec.Containers; len(containers) > 0 {
		spec = containers[0]
	}
	env := []string{}
	for _, e := range spec.Env {
		env = append(env, e.Name+"="+e.Value)
	}
	// lbIP may be a hostname or empty; only an address is an endpoint IP.
	ip, _ := netip.ParseAddr(lbIP)
	endpoints := map[string]*network.EndpointSettings{}
	for name, id := range networkIDs(d) {
		endpoints[name] = &network.EndpointSettings{NetworkID: id, IPAddress: ip}
	}

	user, capAdd, capDrop, privileged, readOnly, securityOpt := dockerSecurityContext(spec.SecurityContext)
	groupAdd, sysctls := dockerPodSecurityContext(d.Spec.Template.Spec.SecurityContext)
	dnsSearch, dnsOptions := dockerDNSConfig(d.Spec.Template.Spec.DNSConfig)
	shmSize, tmpfs := dockerTmpfs(d)

	hostConfig := &container.HostConfig{
		LogConfig: container.LogConfig{
			Type: "json-file",
		},
		RestartPolicy: container.RestartPolicy{Name: restartPolicyOf(d)},
		AutoRemove:    d.Annotations[types.AnnotationAutoRemove] == "true",
		// Each pod has its own cgroup namespace, as with Docker's default on cgroup v2.
		CgroupnsMode:   container.CgroupnsModePrivate,
		Resources:      dockerResources(spec.Resources),
		CapAdd:         capAdd,
		CapDrop:        capDrop,
		Privileged:     privileged,
		ReadonlyRootfs: readOnly,
		GroupAdd:       groupAdd,
		Sysctls:        sysctls,
		SecurityOpt:    securityOpt,
		ExtraHosts:     dockerExtraHosts(d.Spec.Template.Spec.HostAliases),
		DNSSearch:      dnsSearch,
		DNSOptions:     dnsOptions,
		ShmSize:        shmSize,
		Tmpfs:          tmpfs,
	}
	hostConfig.Binds, hostConfig.Mounts = volumeConfig(d)
	process := append(append([]string{}, spec.Command...), spec.Args...)
	var path string
	var args []string
	if len(process) > 0 {
		path, args = process[0], process[1:]
	}

	portMap := network.PortMap{}
	for _, m := range publishedPorts(d) {
		if p, err := network.ParsePort(fmt.Sprintf("%d/%s", m.ContainerPort, strings.ToLower(m.Protocol))); err == nil {
			portMap[p] = append(portMap[p], network.PortBinding{HostIP: netip.IPv4Unspecified(), HostPort: strconv.Itoa(m.HostPort)})
		}
	}
	if d.Annotations[types.AnnotationPortMappings] != "" {
		hostConfig.PortBindings = portMap
	}

	var stopTimeout *int
	if grace := d.Spec.Template.Spec.TerminationGracePeriodSeconds; grace != nil {
		seconds := int(*grace)
		stopTimeout = &seconds
	}

	return container.InspectResponse{
		ID:           containerID(d.UID),
		Name:         "/" + dockerName(d),
		Image:        d.Annotations[types.AnnotationImageRef],
		Created:      d.CreationTimestamp.Format(time.RFC3339),
		Path:         path,
		Args:         args,
		State:        &state,
		RestartCount: containerRestartCount(d, pods),
		HostConfig:   hostConfig,
		Mounts:       containerMounts(d),
		Config: &container.Config{
			Image:        d.Annotations[types.AnnotationImageRef],
			User:         user,
			WorkingDir:   spec.WorkingDir,
			Hostname:     d.Spec.Template.Spec.Hostname,
			StopTimeout:  stopTimeout,
			ExposedPorts: network.PortSet{},
			Tty:          spec.TTY,
			OpenStdin:    spec.Stdin,
			AttachStdin:  false,
			AttachStdout: true,
			AttachStderr: true,
			Cmd:          spec.Args,
			Entrypoint:   spec.Command,
			Env:          env,
			Labels:       dockerLabels(d),
			Healthcheck:  healthConfig(spec.ReadinessProbe),
		},
		NetworkSettings: &container.NetworkSettings{
			Ports:    portMap,
			Networks: endpoints,
		},
	}
}

// humanizeDuration formats a duration the way Docker does in docker ps STATUS:
// seconds, minutes, hours up to 47h, then days, weeks, months, years.
func humanizeDuration(d time.Duration) string {
	seconds := int(d.Seconds())
	if seconds < 60 {
		return fmt.Sprintf("%d seconds", seconds)
	}
	minutes := seconds / 60
	if minutes < 60 {
		return fmt.Sprintf("%d minutes", minutes)
	}
	hours := minutes / 60
	if hours < 48 {
		return fmt.Sprintf("%d hours", hours)
	}
	days := hours / 24
	if days < 14 {
		return fmt.Sprintf("%d days", days)
	}
	weeks := days / 7
	if weeks < 8 {
		return fmt.Sprintf("%d weeks", weeks)
	}
	months := days / 30
	if months < 24 {
		return fmt.Sprintf("%d months", months)
	}
	years := days / 365
	return fmt.Sprintf("%d years", years)
}

// encodePortMappings serialises the raw port binding strings into a JSON label value.
func encodePortMappings(bindings []string, publishAll bool) (string, error) {
	if publishAll {
		b, err := json.Marshal([]string{"-P"})
		return string(b), err
	}
	if len(bindings) == 0 {
		return "", nil
	}
	b, err := json.Marshal(bindings)
	return string(b), err
}

// containerSecurityContext translates the user, capabilities, privileged
// and read-only root settings, or returns nil when none is set. Whether they
// are allowed is decided by the namespace's Pod Security policy.
func containerSecurityContext(opts RunOptions) *corev1.SecurityContext {
	if opts.RunAsUser == nil && opts.RunAsGroup == nil && len(opts.CapAdd) == 0 && len(opts.CapDrop) == 0 && !opts.Privileged && !opts.ReadOnly &&
		!opts.NoNewPrivileges && opts.Seccomp == nil && opts.AppArmor == nil && opts.SELinux == nil {
		return nil
	}
	sc := &corev1.SecurityContext{RunAsUser: opts.RunAsUser, RunAsGroup: opts.RunAsGroup,
		SeccompProfile: opts.Seccomp, AppArmorProfile: opts.AppArmor, SELinuxOptions: opts.SELinux}
	if opts.NoNewPrivileges {
		sc.AllowPrivilegeEscalation = new(bool)
	}
	if opts.Privileged {
		sc.Privileged = &opts.Privileged
	}
	if opts.ReadOnly {
		sc.ReadOnlyRootFilesystem = &opts.ReadOnly
	}
	if len(opts.CapAdd) > 0 || len(opts.CapDrop) > 0 {
		sc.Capabilities = &corev1.Capabilities{}
		for _, c := range opts.CapAdd {
			sc.Capabilities.Add = append(sc.Capabilities.Add, corev1.Capability(strings.TrimPrefix(strings.ToUpper(c), "CAP_")))
		}
		for _, c := range opts.CapDrop {
			sc.Capabilities.Drop = append(sc.Capabilities.Drop, corev1.Capability(strings.TrimPrefix(strings.ToUpper(c), "CAP_")))
		}
	}
	return sc
}

// podSecurityContext returns the pod-level security settings, if any.
func podSecurityContext(opts RunOptions) *corev1.PodSecurityContext {
	if len(opts.Sysctls) == 0 && len(opts.SupplementalGroups) == 0 {
		return nil
	}
	return &corev1.PodSecurityContext{Sysctls: opts.Sysctls, SupplementalGroups: opts.SupplementalGroups}
}

// dnsConfig returns the DNS search domains and options the container adds
// to the cluster's.
func dnsConfig(opts RunOptions) *corev1.PodDNSConfig {
	if len(opts.DNSSearch) == 0 && len(opts.DNSOptions) == 0 {
		return nil
	}
	return &corev1.PodDNSConfig{Searches: opts.DNSSearch, Options: opts.DNSOptions}
}

// containerID returns the Docker ID of the container a Deployment backs: 64
// hex characters, as Docker IDs are, derived from the Deployment's UID so it
// is stable for the container's lifetime and unique.
func containerID(uid k8stypes.UID) string {
	sum := sha256.Sum256([]byte(uid))
	return hex.EncodeToString(sum[:])
}

// dockerName returns the Docker name of a container, falling back to the
// Deployment name for containers created before the name was recorded.
func dockerName(d appsv1.Deployment) string {
	return recordedName(d.ObjectMeta)
}

// recordedName returns the Docker name recorded on an object, falling back to
// the object's name.
func recordedName(m metav1.ObjectMeta) string {
	if name := m.Annotations[types.AnnotationDockerName]; name != "" {
		return name
	}
	return m.Name
}

// dockerLabels returns the Docker labels exactly as the container was created
// with, falling back to the Deployment labels for older containers.
func dockerLabels(d appsv1.Deployment) map[string]string {
	return recordedLabels(d.ObjectMeta)
}

// recordedLabels returns the Docker labels recorded on an object, falling
// back to its Kubernetes labels for objects created before they were recorded.
func recordedLabels(m metav1.ObjectMeta) map[string]string {
	var labels map[string]string
	if raw := m.Annotations[types.AnnotationDockerLabels]; raw != "" && json.Unmarshal([]byte(raw), &labels) == nil {
		return labels
	}
	if _, recorded := m.Annotations[types.AnnotationDockerName]; recorded {
		return map[string]string{}
	}
	return m.Labels
}

// dockerNameInUse reports whether a managed container already has the Docker name.
func (a *KubernetesDockerAdapter) dockerNameInUse(ctx context.Context, name string) (bool, error) {
	list, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return false, fmt.Errorf("unable to list deployments: %w", err)
	}
	for _, d := range list.Items {
		if dockerName(d) == name {
			return true, nil
		}
	}
	return false, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// publishedServiceName returns the name of the Service that publishes a
// container's ports. It must differ from the container's DNS Service, which
// carries the container's own name.
func publishedServiceName(name string) string {
	return strings.TrimRight(truncate(serviceName(name), 60), "-") + "-lb"
}

// namedVolumeMounts translates Docker binds into PVC mounts, and returns the
// Docker volume name of each PVC. Missing named volumes are created, as Docker
// does on run. Host path binds are not supported and produce a warning.
func (a *KubernetesDockerAdapter) namedVolumeMounts(ctx context.Context, binds []string) ([]corev1.Volume, []corev1.VolumeMount, map[string]string, []string, error) {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	names := map[string]string{}
	var warnings []string
	for _, bind := range binds {
		parts := strings.Split(bind, ":")
		if len(parts) < 2 {
			return nil, nil, nil, nil, errdefs.InvalidParameter(fmt.Errorf("invalid volume specification: %q", bind))
		}
		source, target := parts[0], parts[1]
		if strings.HasPrefix(source, "/") || strings.HasPrefix(source, ".") {
			warnings = append(warnings, fmt.Sprintf("bind mount %q ignored: host paths are not supported by d2k", bind))
			continue
		}

		// Creating a volume that exists returns it, as in Docker.
		if _, err := a.CreateVolume(ctx, CreateVolumeOptions{Name: source}); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("unable to create volume %q: %w", source, err)
		}
		claim := sanitiseResourceName(source)
		names[claim] = source

		name := fmt.Sprintf("volume-%d", len(volumes))
		volumes = append(volumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      name,
			MountPath: target,
			ReadOnly:  len(parts) > 2 && slices.Contains(strings.Split(parts[2], ","), "ro"),
		})
	}
	return volumes, mounts, names, warnings, nil
}

// ignoredAliases warns about network aliases d2k cannot honour: a container
// is resolvable by its own name and by its compose service name only.
func ignoredAliases(aliases []string, dockerName, service string) []string {
	var warnings []string
	var seen []string
	for _, alias := range aliases {
		if alias == dockerName || alias == service || slices.Contains(seen, alias) {
			continue
		}
		seen = append(seen, alias)
		warnings = append(warnings, fmt.Sprintf("network alias %q ignored: d2k resolves containers by name and compose service name only", alias))
	}
	return warnings
}

// projectDNSName returns the name of a compose project's headless DNS Service,
// which is also the pod subdomain of the project's containers.
func projectDNSName(project string) string {
	return strings.TrimRight(truncate("compose-"+sanitiseResourceName(project), 63), "-")
}

// scopeDNSToProject makes the container resolvable as <service> by the other
// containers of its compose project, and only by them. Docker scopes names to
// the project's networks; a namespace is a single DNS scope, so each project
// gets its own subdomain, searched before the namespace.
func (a *KubernetesDockerAdapter) scopeDNSToProject(d *appsv1.Deployment, project, service string) []string {
	hostname := sanitiseResourceName(service)
	if len(validation.IsDNS1123Label(hostname)) > 0 {
		return []string{fmt.Sprintf("compose service %q is not a valid DNS name and will not be resolvable by name", service)}
	}

	pod := &d.Spec.Template
	d.Labels[types.LabelComposeProject] = sanitiseResourceName(project)
	pod.Labels[types.LabelComposeProject] = sanitiseResourceName(project)
	pod.Spec.Hostname = hostname
	pod.Spec.Subdomain = projectDNSName(project)
	if a.clusterDNS == "" || a.clusterDomain == "" {
		return nil
	}

	svcDomain := "svc." + a.clusterDomain
	ndots := "5"
	own := pod.Spec.DNSConfig
	if own == nil {
		own = &corev1.PodDNSConfig{}
	}
	pod.Spec.DNSPolicy = corev1.DNSNone
	pod.Spec.DNSConfig = &corev1.PodDNSConfig{
		Nameservers: []string{a.clusterDNS},
		Searches: append([]string{
			pod.Spec.Subdomain + "." + a.namespace + "." + svcDomain,
			a.namespace + "." + svcDomain,
			svcDomain,
			a.clusterDomain,
		}, own.Searches...),
		// The container's own options come last, so they win.
		Options: append([]corev1.PodDNSConfigOption{{Name: "ndots", Value: &ndots}}, own.Options...),
	}
	return nil
}

// ensureProjectDNSService creates the headless Service that publishes the
// hostnames of a compose project's pods under the project's subdomain, and
// adds the container's Deployment to its owners: Kubernetes deletes the
// Service once the project's last container is removed.
func (a *KubernetesDockerAdapter) ensureProjectDNSService(ctx context.Context, project string, owner *appsv1.Deployment) error {
	label := sanitiseResourceName(project)
	name := projectDNSName(project)
	ownerRef := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: owner.Name, UID: owner.UID}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: a.namespace,
			Labels: map[string]string{
				types.LabelManagedBy:      types.LabelManagedByValue,
				types.LabelComposeProject: label,
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			Selector:                 map[string]string{types.LabelComposeProject: label},
			PublishNotReadyAddresses: true,
		},
	}
	_, err := a.client.CoreV1().Services(a.namespace).Create(ctx, svc, metav1.CreateOptions{})
	if !errors.IsAlreadyExists(err) {
		return err
	}

	// Containers of one project are created concurrently.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := a.client.CoreV1().Services(a.namespace).Get(ctx, name, metav1GetOptions())
		if err != nil {
			return fmt.Errorf("unable to get Service %q: %w", name, err)
		}
		if existing.Labels[types.LabelManagedBy] != types.LabelManagedByValue || existing.Labels[types.LabelComposeProject] != label {
			return errdefs.Conflict(fmt.Errorf("service %q, needed for the DNS of compose project %q, is already in use", name, project))
		}
		existing.OwnerReferences = append(existing.OwnerReferences, ownerRef)
		_, err = a.client.CoreV1().Services(a.namespace).Update(ctx, existing, metav1.UpdateOptions{})
		return err
	})
}

// containerNetworks returns the names of the networks a container is attached to.
func containerNetworks(d appsv1.Deployment) []string {
	var networks []string
	if raw := d.Annotations[types.AnnotationNetworks]; raw != "" && json.Unmarshal([]byte(raw), &networks) == nil {
		return networks
	}
	return []string{"bridge"}
}

// volumeClaims maps the pod volumes of a container's named volumes to the
// volumes' Docker names.
func volumeClaims(d appsv1.Deployment) map[string]string {
	var names map[string]string
	if raw := d.Annotations[types.AnnotationVolumeNames]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &names)
	}
	claims := map[string]string{}
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claims[v.Name] = cmp.Or(names[v.PersistentVolumeClaim.ClaimName], v.PersistentVolumeClaim.ClaimName)
		}
	}
	return claims
}

// volumeConfig returns the container's named-volume mounts as the
// HostConfig that creates them: binds, or mounts for those with a subpath,
// which a bind cannot express.
func volumeConfig(d appsv1.Deployment) ([]string, []mount.Mount) {
	if len(d.Spec.Template.Spec.Containers) == 0 {
		return nil, nil
	}
	claims := volumeClaims(d)
	var binds []string
	var mounts []mount.Mount
	for _, m := range d.Spec.Template.Spec.Containers[0].VolumeMounts {
		claim, ok := claims[m.Name]
		switch {
		case !ok:
		case m.SubPath != "":
			mounts = append(mounts, mount.Mount{Type: mount.TypeVolume, Source: claim, Target: m.MountPath, ReadOnly: m.ReadOnly,
				VolumeOptions: &mount.VolumeOptions{Subpath: m.SubPath}})
		case m.ReadOnly:
			binds = append(binds, claim+":"+m.MountPath+":ro")
		default:
			binds = append(binds, claim+":"+m.MountPath)
		}
	}
	return binds, mounts
}

// dockerTmpfs returns the container's memory-backed mounts as the
// HostConfig that creates them: ShmSize for /dev/shm, Tmpfs for the rest.
func dockerTmpfs(d appsv1.Deployment) (shmSize int64, tmpfs map[string]string) {
	if len(d.Spec.Template.Spec.Containers) == 0 {
		return 0, nil
	}
	volumes := map[string]*corev1.EmptyDirVolumeSource{}
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory {
			volumes[v.Name] = v.EmptyDir
		}
	}
	for _, m := range d.Spec.Template.Spec.Containers[0].VolumeMounts {
		v, ok := volumes[m.Name]
		if !ok {
			continue
		}
		var size string
		if v.SizeLimit != nil {
			size = "size=" + strconv.FormatInt(v.SizeLimit.Value(), 10)
		}
		if m.MountPath == "/dev/shm" && v.SizeLimit != nil {
			shmSize = v.SizeLimit.Value()
			continue
		}
		if tmpfs == nil {
			tmpfs = map[string]string{}
		}
		tmpfs[m.MountPath] = size
	}
	return shmSize, tmpfs
}

// containerMounts returns the container's named-volume mounts.
func containerMounts(d appsv1.Deployment) []container.MountPoint {
	claims := volumeClaims(d)
	mounts := []container.MountPoint{}
	if len(d.Spec.Template.Spec.Containers) == 0 {
		return mounts
	}
	for _, m := range d.Spec.Template.Spec.Containers[0].VolumeMounts {
		if claim, ok := claims[m.Name]; ok {
			mounts = append(mounts, container.MountPoint{
				Type:        mount.TypeVolume,
				Name:        claim,
				Driver:      "d2k",
				Destination: m.MountPath,
				RW:          !m.ReadOnly,
			})
		}
	}
	return mounts
}

// ConnectNetwork records that a container joined (or left) a network. The
// namespace network is flat, so only the reported attachment changes.
func (a *KubernetesDockerAdapter) ConnectNetwork(ctx context.Context, networkNameOrID, containerNameOrID string, connect bool) error {
	resolved, err := a.resolveDeploymentName(ctx, containerNameOrID)
	if err != nil {
		return err
	}

	name := strings.TrimPrefix(networkNameOrID, networkIDForName("", a.namespace))

	// Portainer reconnects a container's networks with one request per
	// network, concurrently; retry so a losing request's own read isn't
	// stale by the time it writes back the full network list.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		d, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, resolved, metav1GetOptions())
		if err != nil {
			return fmt.Errorf("unable to get deployment %q: %w", resolved, err)
		}

		networks := slices.DeleteFunc(containerNetworks(*d), func(n string) bool { return n == name })
		if connect {
			networks = append(networks, name)
		}
		encoded, err := json.Marshal(networks)
		if err != nil {
			return err
		}
		if d.Annotations == nil {
			d.Annotations = map[string]string{}
		}
		d.Annotations[types.AnnotationNetworks] = string(encoded)
		_, err = a.client.AppsV1().Deployments(a.namespace).Update(ctx, d, metav1.UpdateOptions{})
		return err
	})
}
