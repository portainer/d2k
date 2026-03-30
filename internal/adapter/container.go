package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/portainer/d2k/internal/types"
	"github.com/portainer/d2k/pkg/portmapper"
)

// RunOptions mirrors the subset of docker run flags that d2k supports.
type RunOptions struct {
	// Name is the --name flag value.
	Name string
	// Image is the container image reference.
	Image string
	// Cmd overrides the image default command.
	Cmd []string
	// Env is a list of KEY=VALUE environment variable strings.
	Env []string
	// Labels are user-supplied labels (-l / --label).
	Labels map[string]string
	// PortBindings are raw -p flag values, e.g. "8080:80", "127.0.0.1:443:443".
	PortBindings []string
	// PublishAll corresponds to -P.
	PublishAll bool
	// ExposedPorts are the ports declared in the image config, used with -P.
	ExposedPorts map[string]struct{}
	// Volumes are -v flag values (host:container or just container path).
	// Phase 1: stored as annotation only; volume mounting is a follow-on.
	Volumes []string
}

// ContainerSummary is a Docker-compatible summary row, as returned by docker ps.
type ContainerSummary struct {
	ID      string
	Names   []string
	Image   string
	Status  string
	State   string
	Created int64
	Ports   []dockertypes.Port
	Labels  map[string]string
}

// CreateContainer implements docker run: creates a Deployment and, where
// required, a Service in the target namespace.
func (a *KubernetesDockerAdapter) CreateContainer(ctx context.Context, opts RunOptions) (string, []string, error) {
	if opts.Name == "" {
		return "", nil, fmt.Errorf("container name is required")
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

	// Build and create the Deployment.
	deployment, err := a.buildDeployment(opts, kind, mappings)
	if err != nil {
		return "", nil, fmt.Errorf("unable to build deployment: %w", err)
	}

	created, err := a.client.AppsV1().Deployments(a.namespace).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		return "", nil, fmt.Errorf("unable to create deployment %q: %w", opts.Name, err)
	}

	// Create the Service if needed.
	if kind != portmapper.NoService {
		svc, svcErr := a.buildService(opts.Name, kind, mappings)
		if svcErr != nil {
			return "", nil, fmt.Errorf("unable to build service: %w", svcErr)
		}
		if _, svcErr = a.client.CoreV1().Services(a.namespace).Create(ctx, svc, metav1.CreateOptions{}); svcErr != nil {
			// Roll back the Deployment so we don't leave an orphan.
			_ = a.client.AppsV1().Deployments(a.namespace).Delete(ctx, opts.Name, metav1.DeleteOptions{})
			return "", nil, fmt.Errorf("unable to create service for %q: %w", opts.Name, svcErr)
		}
	}

	return string(created.UID), warnings, nil
}

// ListContainers implements docker ps.
// When all is false, only Deployments with at least one ready replica are returned (running containers).
// When all is true, all d2k-managed Deployments are returned.
func (a *KubernetesDockerAdapter) ListContainers(ctx context.Context, all bool) ([]ContainerSummary, error) {
	deployments, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return nil, fmt.Errorf("unable to list deployments: %w", err)
	}

	var summaries []ContainerSummary

	for _, d := range deployments.Items {
		if !all && d.Status.ReadyReplicas == 0 {
			continue
		}
		summaries = append(summaries, deploymentToSummary(d))
	}

	return summaries, nil
}

// StopContainer implements docker stop: scales the Deployment to 0 replicas.
func (a *KubernetesDockerAdapter) StopContainer(ctx context.Context, name string) error {
	return a.scaleDeployment(ctx, name, 0)
}

// StartContainer implements docker start: scales the Deployment back to 1 replica.
func (a *KubernetesDockerAdapter) StartContainer(ctx context.Context, name string) error {
	return a.scaleDeployment(ctx, name, 1)
}

// RemoveContainer implements docker rm: deletes the Deployment and its associated Service (if any).
func (a *KubernetesDockerAdapter) RemoveContainer(ctx context.Context, name string) error {
	resolved, err := a.resolveDeploymentName(ctx, name)
	if err != nil {
		return err
	}

	if err := a.client.AppsV1().Deployments(a.namespace).Delete(ctx, resolved, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("unable to delete deployment %q: %w", resolved, err)
	}

	// Best-effort Service deletion — the Service may not exist.
	svcErr := a.client.CoreV1().Services(a.namespace).Delete(ctx, resolved, metav1.DeleteOptions{})
	if svcErr != nil && !errors.IsNotFound(svcErr) {
		a.logger.Warnw("unable to delete service", "name", resolved, "error", svcErr)
	}

	return nil
}

// InspectContainer implements docker inspect for a single container.
func (a *KubernetesDockerAdapter) InspectContainer(ctx context.Context, name string) (*dockertypes.ContainerJSON, error) {
	resolved, err := a.resolveDeploymentName(ctx, name)
	if err != nil {
		return nil, err
	}

	d, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, resolved, metav1GetOptions())
	if err != nil {
		return nil, fmt.Errorf("unable to get deployment %q: %w", resolved, err)
	}

	json := deploymentToContainerJSON(*d)
	return &json, nil
}

// --- builders ---

func (a *KubernetesDockerAdapter) buildDeployment(opts RunOptions, kind portmapper.MappingKind, mappings []portmapper.PortMapping) (*appsv1.Deployment, error) {
	labels := managedLabels(opts.Name)
	maps.Copy(labels, opts.Labels)

	// Encode port mappings as an annotation so we can reconstruct them later.
	// Annotations (unlike labels) accept arbitrary string values, which is required
	// because the JSON-encoded port mappings contain characters like ':' and '['.
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

	// Build container ports for the pod spec.
	var containerPorts []corev1.ContainerPort
	for _, m := range mappings {
		containerPorts = append(containerPorts, corev1.ContainerPort{
			ContainerPort: int32(m.ContainerPort),
			Protocol:      corev1.Protocol(m.Protocol),
		})
	}

	// Build env vars.
	var envVars []corev1.EnvVar
	for _, e := range opts.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) != 2 {
			continue
		}
		envVars = append(envVars, corev1.EnvVar{Name: parts[0], Value: parts[1]})
	}

	replicas := int32(1)

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
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    opts.Name,
							Image:   opts.Image,
							Command: opts.Cmd,
							Env:     envVars,
							Ports:   containerPorts,
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
		// For NodePort the scheduler assigns the node port; for LB we set it as the Service port.
		// HostPort=0 means auto-assign (used by -P).
		if m.HostPort == 0 {
			sp.Port = int32(m.ContainerPort)
		}
		ports = append(ports, sp)
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: a.namespace,
			Labels:    managedLabels(name),
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: map[string]string{"app": name},
			Ports:    ports,
		},
	}, nil
}

// --- name/ID resolution ---

// resolveDeploymentName maps a Docker container name or UID to a Kubernetes Deployment
// name. After CreateContainer the Docker CLI uses the returned UID for every follow-up
// call (start, wait, inspect), so we must accept both forms.
// Fast path: try the value directly as a Deployment name.
// Slow path: list all managed Deployments and match by UID.
func (a *KubernetesDockerAdapter) resolveDeploymentName(ctx context.Context, nameOrID string) (string, error) {
	_, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, nameOrID, metav1GetOptions())
	if err == nil {
		return nameOrID, nil
	}
	if !errors.IsNotFound(err) {
		return "", fmt.Errorf("unable to look up container %q: %w", nameOrID, err)
	}
	list, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return "", fmt.Errorf("unable to list deployments: %w", err)
	}
	for _, d := range list.Items {
		if string(d.UID) == nameOrID {
			return d.Name, nil
		}
	}
	return "", fmt.Errorf("container %q not found", nameOrID)
}

// --- scale helper ---

func (a *KubernetesDockerAdapter) scaleDeployment(ctx context.Context, name string, replicas int32) error {
	resolved, err := a.resolveDeploymentName(ctx, name)
	if err != nil {
		return err
	}
	d, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, resolved, metav1GetOptions())
	if err != nil {
		return fmt.Errorf("unable to get deployment %q: %w", resolved, err)
	}

	d.Spec.Replicas = &replicas
	_, err = a.client.AppsV1().Deployments(a.namespace).Update(ctx, d, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("unable to scale deployment %q to %d: %w", name, replicas, err)
	}

	return nil
}

// --- converters ---

func deploymentToSummary(d appsv1.Deployment) ContainerSummary {
	state := "exited"
	status := "Exited (0)"
	if d.Spec.Replicas != nil && *d.Spec.Replicas > 0 {
		if d.Status.ReadyReplicas > 0 {
			state = "running"
			uptime := time.Since(d.CreationTimestamp.Time).Round(time.Second)
			status = fmt.Sprintf("Up %s", uptime)
		} else {
			state = "starting"
			status = "Starting"
		}
	}

	return ContainerSummary{
		ID:      string(d.UID),
		Names:   []string{"/" + d.Name},
		Image:   d.Annotations[types.AnnotationImageRef],
		Status:  status,
		State:   state,
		Created: d.CreationTimestamp.Unix(),
		Labels:  d.Labels,
	}
}

func deploymentToContainerJSON(d appsv1.Deployment) dockertypes.ContainerJSON {
	running := d.Status.ReadyReplicas > 0

	state := &dockertypes.ContainerState{
		Status:  "exited",
		Running: false,
	}
	if running {
		state.Status = "running"
		state.Running = true
		startedAt := d.CreationTimestamp.Time.Format(time.RFC3339)
		state.StartedAt = startedAt
	}

	hostConfig := &container.HostConfig{}

	// Reconstruct port bindings from annotations for the inspect response.
	portMap := nat.PortMap{}
	rawPorts := d.Annotations[types.AnnotationPortMappings]
	if rawPorts != "" {
		var bindings []string
		if err := json.Unmarshal([]byte(rawPorts), &bindings); err == nil {
			for _, raw := range bindings {
				parts := strings.SplitN(raw, ":", 2)
				if len(parts) == 2 {
					p := nat.Port(parts[1] + "/tcp")
					portMap[p] = []nat.PortBinding{{HostPort: parts[0]}}
				}
			}
		}
		hostConfig.PortBindings = portMap
	}

	return dockertypes.ContainerJSON{
		ContainerJSONBase: &dockertypes.ContainerJSONBase{
			ID:         string(d.UID),
			Name:       "/" + d.Name,
			Image:      d.Annotations[types.AnnotationImageRef],
			Created:    d.CreationTimestamp.Time.Format(time.RFC3339),
			State:      state,
			HostConfig: hostConfig,
		},
		Config: &container.Config{
			Image: d.Annotations[types.AnnotationImageRef],
		},
		NetworkSettings: &dockertypes.NetworkSettings{},
	}
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
