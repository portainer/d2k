// swarm.go contains the Kubernetes adapter methods that back the Swarm API surface.
//
// Each method is stubbed to compile and return sensible empty responses.
// Implementation follows in subsequent commits once the wiring is confirmed working.
//
// Identity model:
//   - Swarm cluster ID, manager node ID, and join tokens are derived once from the
//     Kubernetes cluster UID and stored in a ConfigMap named d2k-identity in the
//     configured namespace. This makes the identity stable across d2k restarts.
//
// Label model:
//   - All resources created via the Swarm surface carry:
//       d2k.portainer.io/swarm-managed-by = d2k
//       d2k.portainer.io/swarm-stack      = <stack name>    (when part of a stack)
//       d2k.portainer.io/swarm-service    = <service name>
//   - Swarm-format IDs are stored as annotations so inspect responses return
//     stable IDs that round-trip correctly through Docker CLI / toolchains.
package adapter

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/portainer/d2k/internal/types"
)

// swarmID converts a Kubernetes UID (UUID format) into a Swarm-style ID:
// 25 uppercase alphanumeric characters, matching the format Docker Swarm uses.
// The conversion is deterministic - the same UID always produces the same swarm ID.
func swarmID(uid string) string {
	// Strip hyphens from UUID, uppercase, truncate/pad to 25 chars.
	s := strings.ToUpper(strings.ReplaceAll(uid, "-", ""))
	if len(s) > 25 {
		s = s[:25]
	}
	for len(s) < 25 {
		s += "0"
	}
	return s
}

// SwarmIdentity returns the stable Swarm cluster identity, creating it from the
// cluster UID if it does not yet exist in the d2k-identity ConfigMap.
func (a *KubernetesDockerAdapter) SwarmIdentity(ctx context.Context) (map[string]any, error) {
	cm, err := a.client.CoreV1().ConfigMaps(a.namespace).Get(ctx, types.ConfigMapSwarmIdentity, metav1.GetOptions{})
	if err == nil {
		// ConfigMap exists - deserialise and return.
		var identity map[string]any
		if raw, ok := cm.Data["identity"]; ok {
			if jsonErr := json.Unmarshal([]byte(raw), &identity); jsonErr == nil {
				return identity, nil
			}
		}
	}

	// Derive identity from the d2k namespace UID, which is stable for the
	// lifetime of the namespace and requires no permissions outside our scope.
	ns, err := a.client.CoreV1().Namespaces().Get(ctx, a.namespace, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("unable to read namespace %q for cluster UID: %w", a.namespace, err)
	}

	clusterUID := string(ns.UID)
	swarmClusterID := swarmID(clusterUID)
	// Manager node ID is derived from cluster UID with a salt so it differs from
	// the cluster ID itself.
	managerNodeID := swarmID(strings.ToUpper(clusterUID) + "MGR")

	identity := map[string]any{
		"ID":      swarmClusterID,
		"NodeID":  managerNodeID,
		"Version": map[string]any{"Index": uint64(1)},
		"CreatedAt": "2024-01-01T00:00:00.000000000Z",
		"UpdatedAt": "2024-01-01T00:00:00.000000000Z",
		"Spec": map[string]any{
			"Name":                 "d2k",
			"Labels":               map[string]string{},
			"Orchestration":        map[string]any{"TaskHistoryRetentionLimit": 5},
			"Raft":                 map[string]any{"SnapshotInterval": 10000, "HeartbeatTick": 1, "ElectionTick": 10},
			"Dispatcher":           map[string]any{"HeartbeatPeriod": 5000000000},
			"CAConfig":             map[string]any{},
			"TaskDefaults":         map[string]any{},
			"EncryptionConfig":     map[string]any{"AutoLockManagers": false},
		},
		"TLSInfo": map[string]any{
			"TrustRoot":           "",
			"CertIssuerSubject":   "",
			"CertIssuerPublicKey": "",
		},
		"RootRotationInProgress": false,
		"DefaultAddrPool":        []string{"10.0.0.0/8"},
		"SubnetSize":             24,
		"DataPathPort":           4789,
		"JoinTokens": map[string]string{
			"Worker":  "SWMTKN-1-" + swarmClusterID + "-worker",
			"Manager": "SWMTKN-1-" + swarmClusterID + "-manager",
		},
	}

	// Persist to ConfigMap so identity is stable across restarts.
	raw, _ := json.Marshal(identity)
	newCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      types.ConfigMapSwarmIdentity,
			Namespace: a.namespace,
			Labels: map[string]string{
				types.LabelManagedBy:      types.LabelManagedByValue,
				types.LabelSwarmManagedBy: types.LabelSwarmManagedByValue,
			},
		},
		Data: map[string]string{"identity": string(raw)},
	}
	// Best-effort create - if it races with another instance, that's fine.
	_, _ = a.client.CoreV1().ConfigMaps(a.namespace).Create(ctx, newCM, metav1.CreateOptions{})

	return identity, nil
}

// SwarmListNodes lists Kubernetes nodes translated to the Swarm node shape.
func (a *KubernetesDockerAdapter) SwarmListNodes(ctx context.Context) ([]map[string]any, error) {
	nodeList, err := a.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("unable to list nodes: %w", err)
	}

	result := make([]map[string]any, 0, len(nodeList.Items))
	for _, n := range nodeList.Items {
		result = append(result, kubeNodeToSwarm(n, a.apiServerHost))
	}
	return result, nil
}

// SwarmInspectNode returns a single Kubernetes node as a Swarm node.
// id may be the Swarm-format ID (25-char uppercase, derived from UID) or
// the Kubernetes node name. We always list and check both - the Docker CLI
// passes the swarm ID from node ls back into node inspect, so a direct Get
// by name would fail for that case.
func (a *KubernetesDockerAdapter) SwarmInspectNode(ctx context.Context, id string) (map[string]any, error) {
	nodeList, err := a.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("unable to list nodes: %w", err)
	}
	for _, n := range nodeList.Items {
		if n.Name == id || swarmID(string(n.UID)) == id {
			return kubeNodeToSwarm(n, a.apiServerHost), nil
		}
	}
	return nil, fmt.Errorf("node %q not found", id)
}

// SwarmUpdateNode handles node availability changes (drain / active / pause).
func (a *KubernetesDockerAdapter) SwarmUpdateNode(ctx context.Context, id string, body io.Reader) error {
	var req struct {
		Availability string `json:"Availability"`
	}
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		return fmt.Errorf("invalid node update request: %w", err)
	}

	// Resolve by name first, then by swarm ID derived from UID.
	node, err := a.client.CoreV1().Nodes().Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		nodeList, listErr := a.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if listErr != nil {
			return fmt.Errorf("node %q not found", id)
		}
		for _, n := range nodeList.Items {
			if swarmID(string(n.UID)) == id {
				node = &n
				break
			}
		}
		if node == nil {
			return fmt.Errorf("node %q not found", id)
		}
	}

	switch strings.ToLower(req.Availability) {
	case "drain":
		node.Spec.Unschedulable = true
		// TODO: also evict existing pods (kubectl drain semantics)
	case "active":
		node.Spec.Unschedulable = false
	case "pause":
		node.Spec.Unschedulable = true
	}

	_, err = a.client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	return err
}

// swarmServiceSpec is the subset of the Docker Swarm ServiceSpec that d2k translates.
// Only fields that map cleanly to Kubernetes primitives are handled; the rest are
// silently ignored to maintain wire compatibility with existing toolchains.
type swarmServiceSpec struct {
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels"`

	TaskTemplate struct {
		ContainerSpec struct {
			Image   string   `json:"Image"`
			Command []string `json:"Command"`
			Args    []string `json:"Args"`
			Env     []string `json:"Env"`
			Dir     string   `json:"Dir"`
			User    string   `json:"User"`
			// Secrets injected as volume mounts at /run/secrets/<target>
			Secrets []struct {
				File struct {
					Name string `json:"Name"`
					UID  string `json:"UID"`
					GID  string `json:"GID"`
					Mode int    `json:"Mode"`
				} `json:"File"`
				SecretID   string `json:"SecretID"`
				SecretName string `json:"SecretName"`
			} `json:"Secrets"`
			// Configs injected as volume mounts
			Configs []struct {
				File struct {
					Name string `json:"Name"`
					UID  string `json:"UID"`
					GID  string `json:"GID"`
					Mode int    `json:"Mode"`
				} `json:"File"`
				ConfigID   string `json:"ConfigID"`
				ConfigName string `json:"ConfigName"`
			} `json:"Configs"`
			// Mounts: volume, bind, and tmpfs mounts from the compose volumes: key.
			Mounts []struct {
				Type        string `json:"Type"`   // volume | bind | tmpfs
				Source      string `json:"Source"` // volume name or host path
				Target      string `json:"Target"` // container path
				ReadOnly    bool   `json:"ReadOnly"`
				BindOptions *struct {
					Propagation string `json:"Propagation"`
				} `json:"BindOptions"`
				VolumeOptions *struct {
					NoCopy bool              `json:"NoCopy"`
					Labels map[string]string `json:"Labels"`
				} `json:"VolumeOptions"`
			} `json:"Mounts"`
		} `json:"ContainerSpec"`
		Resources struct {
			Limits struct {
				NanoCPUs    int64 `json:"NanoCPUs"`
				MemoryBytes int64 `json:"MemoryBytes"`
			} `json:"Limits"`
			Reservations struct {
				NanoCPUs    int64 `json:"NanoCPUs"`
				MemoryBytes int64 `json:"MemoryBytes"`
			} `json:"Reservations"`
		} `json:"Resources"`
		RestartPolicy struct {
			Condition   string `json:"Condition"`   // none | on-failure | any
			Delay       int64  `json:"Delay"`
			MaxAttempts int64  `json:"MaxAttempts"`
		} `json:"RestartPolicy"`
		Placement struct {
			Constraints []string `json:"Constraints"` // e.g. "node.role == worker"
		} `json:"Placement"`
	} `json:"TaskTemplate"`

	Mode struct {
		Replicated *struct {
			Replicas int64 `json:"Replicas"`
		} `json:"Replicated"`
		Global *struct{} `json:"Global"` // maps to DaemonSet - not supported, warn
	} `json:"Mode"`

	UpdateConfig struct {
		Parallelism int64  `json:"Parallelism"`
		Order       string `json:"Order"` // stop-first | start-first
	} `json:"UpdateConfig"`

	EndpointSpec struct {
		Mode  string `json:"Mode"` // vip | dnsrr
		Ports []struct {
			Protocol      string `json:"Protocol"`
			TargetPort    int    `json:"TargetPort"`
			PublishedPort int    `json:"PublishedPort"`
			PublishMode   string `json:"PublishMode"` // ingress | host
		} `json:"Ports"`
	} `json:"EndpointSpec"`
}

// SwarmCreateService translates a Swarm ServiceSpec into a Kubernetes Deployment
// plus a LoadBalancer Service if ports are published.
func (a *KubernetesDockerAdapter) SwarmCreateService(ctx context.Context, body io.Reader) (map[string]any, error) {
	var spec swarmServiceSpec
	if err := json.NewDecoder(body).Decode(&spec); err != nil {
		return nil, fmt.Errorf("invalid service spec: %w", err)
	}

	name := sanitiseResourceName(spec.Name)
	if name == "" {
		return nil, fmt.Errorf("service Name is required")
	}

	var warnings []string

	cs := spec.TaskTemplate.ContainerSpec

	// --- replicas ---
	replicas := int32(1)
	if spec.Mode.Global != nil {
		// Global mode = DaemonSet. We don't translate DaemonSets - warn and
		// treat as replicated with 1 replica so the service still comes up.
		a.logger.Warnw("global mode service requested; d2k does not support DaemonSet translation, deploying as replicated with 1 replica", "service", name)
	} else if spec.Mode.Replicated != nil && spec.Mode.Replicated.Replicas > 0 {
		replicas = int32(spec.Mode.Replicated.Replicas)
	}

	// --- env ---
	var envVars []corev1.EnvVar
	for _, e := range cs.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envVars = append(envVars, corev1.EnvVar{Name: parts[0], Value: parts[1]})
		}
	}

	// --- resource limits/requests ---
	// Build from whatever the service spec provided first.
	resourceReqs := corev1.ResourceRequirements{}
	if spec.TaskTemplate.Resources.Limits.MemoryBytes > 0 || spec.TaskTemplate.Resources.Limits.NanoCPUs > 0 {
		limits := corev1.ResourceList{}
		if spec.TaskTemplate.Resources.Limits.MemoryBytes > 0 {
			limits[corev1.ResourceMemory] = *resource.NewQuantity(spec.TaskTemplate.Resources.Limits.MemoryBytes, resource.BinarySI)
		}
		if spec.TaskTemplate.Resources.Limits.NanoCPUs > 0 {
			limits[corev1.ResourceCPU] = *resource.NewMilliQuantity(spec.TaskTemplate.Resources.Limits.NanoCPUs/1e6, resource.DecimalSI)
		}
		resourceReqs.Limits = limits
	}
	if spec.TaskTemplate.Resources.Reservations.MemoryBytes > 0 || spec.TaskTemplate.Resources.Reservations.NanoCPUs > 0 {
		requests := corev1.ResourceList{}
		if spec.TaskTemplate.Resources.Reservations.MemoryBytes > 0 {
			requests[corev1.ResourceMemory] = *resource.NewQuantity(spec.TaskTemplate.Resources.Reservations.MemoryBytes, resource.BinarySI)
		}
		if spec.TaskTemplate.Resources.Reservations.NanoCPUs > 0 {
			requests[corev1.ResourceCPU] = *resource.NewMilliQuantity(spec.TaskTemplate.Resources.Reservations.NanoCPUs/1e6, resource.DecimalSI)
		}
		resourceReqs.Requests = requests
	}

	// --- quota-aware request injection ---
	// If the namespace has a ResourceQuota that requires requests.cpu or
	// requests.memory, any pod without those fields will be rejected 403 Forbidden.
	// Docker has no concept of ResourceQuotas so the caller has no way to know they
	// need --reserve-cpu / --reserve-memory. Inspect active quotas and inject the
	// minimum required values when the service spec didn't provide them.
	var quotaErr error
	resourceReqs, quotaErr = a.injectQuotaDefaults(ctx, resourceReqs)
	if quotaErr != nil {
		return nil, quotaErr
	}

	// --- restart policy ---
	// Kubernetes Deployments only support RestartPolicy "Always" on pod templates.
	// "OnFailure" and "Never" are only valid on Jobs/CronJobs. Map all Swarm
	// restart conditions to "Always" — for long-running services (including
	// Jenkins agents) this is the correct behaviour regardless of Swarm condition.
	restartPolicy := corev1.RestartPolicyAlways

	// --- placement constraints -> nodeSelector + nodeAffinity ---
	// Swarm constraint format: "node.role == worker", "node.hostname == mynode",
	// "node.labels.foo == bar". We translate each to the closest Kubernetes
	// equivalent. node.role==worker is special: worker nodes in Kubernetes have
	// no affirmative label, so we use a NotIn affinity on control-plane instead
	// of a nodeSelector that would silently match nothing.
	nodeSelector := map[string]string{}
	var workerAffinity bool
	for _, c := range spec.TaskTemplate.Placement.Constraints {
		c = strings.TrimSpace(c)
		parts := strings.SplitN(c, "==", 2)
		if len(parts) != 2 {
			continue
		}
		lhs := strings.TrimSpace(parts[0])
		rhs := strings.TrimSpace(parts[1])
		switch {
		case lhs == "node.role":
			if rhs == "worker" {
				// Workers have no affirmative label in Kubernetes — use affinity
				// to exclude control-plane nodes instead.
				workerAffinity = true
			} else if rhs == "manager" {
				nodeSelector["node-role.kubernetes.io/control-plane"] = ""
			}
		case lhs == "node.hostname":
			// Map directly to the well-known Kubernetes hostname label.
			nodeSelector["kubernetes.io/hostname"] = rhs
		case strings.HasPrefix(lhs, "node.labels."):
			// Pass user node labels through directly.
			key := strings.TrimPrefix(lhs, "node.labels.")
			nodeSelector[key] = rhs
		}
	}

	// Build NodeAffinity for worker constraint (NotIn control-plane).
	var nodeAffinity *corev1.NodeAffinity
	if workerAffinity {
		nodeAffinity = &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      "node-role.kubernetes.io/control-plane",
								Operator: corev1.NodeSelectorOpDoesNotExist,
							},
						},
					},
				},
			},
		}
	}

	// --- secrets as volume mounts at /run/secrets/<name> ---
	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount
	for _, s := range cs.Secrets {
		// The secret was stored under the sanitised name (underscores → hyphens).
		// Use the same sanitisation here so the volume reference matches.
		k8sSecretName := sanitiseResourceName(s.SecretName)
		targetName := s.File.Name
		if targetName == "" {
			targetName = s.SecretName // keep original as the mount filename
		}
		volumes = append(volumes, corev1.Volume{
			Name: "secret-" + k8sSecretName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: k8sSecretName,
					Items: []corev1.KeyToPath{
						{Key: k8sSecretName, Path: targetName},
					},
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "secret-" + k8sSecretName,
			MountPath: "/run/secrets/" + targetName,
			SubPath:   targetName,
			ReadOnly:  true,
		})
	}

	// --- configs as volume mounts ---
	for _, c := range cs.Configs {
		k8sConfigName := sanitiseResourceName(c.ConfigName)
		targetName := c.File.Name
		if targetName == "" {
			targetName = c.ConfigName // keep original as the mount filename
		}
		volumes = append(volumes, corev1.Volume{
			Name: "config-" + k8sConfigName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: k8sConfigName},
					Items: []corev1.KeyToPath{
						{Key: k8sConfigName, Path: targetName},
					},
				},
			},
		})
		// Ensure mount path is absolute without double-slash if targetName already has a leading slash.
		configMountPath := "/" + strings.TrimPrefix(targetName, "/")
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "config-" + k8sConfigName,
			MountPath: configMountPath,
			SubPath:   targetName,
			ReadOnly:  true,
		})
	}

	// --- Mounts: volume, bind, tmpfs ---
	// Swarm sends compose volumes: entries as Mounts in the service spec.
	// We translate each type to the closest Kubernetes equivalent:
	//   volume  -> PVC (must already exist from the stack deploy volume create step)
	//   bind    -> hostPath
	//   tmpfs   -> emptyDir with medium=Memory
	for _, m := range cs.Mounts {
		mountName := sanitiseResourceName(m.Source)
		if mountName == "" {
			mountName = sanitiseResourceName(strings.TrimPrefix(m.Target, "/"))
		}
		// Ensure volume name is unique if source is empty (e.g. anonymous tmpfs).
		if mountName == "" {
			mountName = fmt.Sprintf("mount-%d", len(volumes))
		}

		switch strings.ToLower(m.Type) {
		case "volume", "":
			if m.Source == "" {
				volumes = append(volumes, corev1.Volume{
					Name:         mountName,
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				})
			} else {
				pvcName := sanitiseResourceName(m.Source)
				// Create a fallback PVC if it doesn't exist yet.
				if _, pvcErr := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Get(ctx, pvcName, metav1.GetOptions{}); pvcErr != nil {
					if errors.IsNotFound(pvcErr) {
						qty := resource.MustParse("1Gi")
						pvcLabels := map[string]string{
							types.LabelManagedBy:      types.LabelManagedByValue,
							types.LabelSwarmManagedBy: types.LabelSwarmManagedByValue,
						}
						pvcSpec := corev1.PersistentVolumeClaimSpec{
							Resources: corev1.VolumeResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
							},
						}
						// If an NFS StorageClass is available, use it with RWX access.
						// This handles docker stack deploy which never calls POST /volumes/create.
						if nfsSC, hasNFS := a.anyNFSStorageClass(); hasNFS {
							a.logger.Infow("creating NFS PVC for volume mount via stack deploy", "pvc", pvcName, "storageClass", nfsSC, "service", name)
							pvcSpec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
							pvcSpec.StorageClassName = &nfsSC
						} else {
							a.logger.Warnw("PVC not found for volume mount; creating fallback PVC", "pvc", pvcName, "service", name)
							pvcSpec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
						}
						fallbackPVC := &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      pvcName,
								Namespace: a.namespace,
								Labels:    pvcLabels,
							},
							Spec: pvcSpec,
						}
						if _, createErr := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Create(ctx, fallbackPVC, metav1.CreateOptions{}); createErr != nil && !errors.IsAlreadyExists(createErr) {
							warnings = append(warnings, fmt.Sprintf("d2k: unable to create PVC for volume %q: %s", pvcName, createErr))
						}
					}
				}
				volumes = append(volumes, corev1.Volume{
					Name: mountName,
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
							ReadOnly:  m.ReadOnly,
						},
					},
				})
			}
		case "bind":
			volumes = append(volumes, corev1.Volume{
				Name: mountName,
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: m.Source},
				},
			})
		case "tmpfs":
			volumes = append(volumes, corev1.Volume{
				Name:         mountName,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}},
			})
		default:
			// Unknown type — skip rather than fail the whole deploy.
			a.logger.Warnw("unsupported mount type; skipping", "type", m.Type, "source", m.Source, "target", m.Target)
			continue
		}

		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      mountName,
			MountPath: m.Target,
			ReadOnly:  m.ReadOnly,
		})
	}

	// --- update strategy ---
	updateStrategy := appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxUnavailable: intstrPtr(intstr.FromInt(0)),
			MaxSurge:       intstrPtr(intstr.FromInt(1)),
		},
	}
	if spec.UpdateConfig.Order == "stop-first" {
		updateStrategy.RollingUpdate = &appsv1.RollingUpdateDeployment{
			MaxUnavailable: intstrPtr(intstr.FromInt(1)),
			MaxSurge:       intstrPtr(intstr.FromInt(0)),
		}
	}
	if spec.UpdateConfig.Parallelism > 0 {
		p := int(spec.UpdateConfig.Parallelism)
		updateStrategy.RollingUpdate.MaxSurge = intstrPtr(intstr.FromInt(p))
	}

	// --- labels ---
	baseLabels := map[string]string{
		types.LabelManagedBy:      types.LabelManagedByValue,
		types.LabelSwarmManagedBy: types.LabelSwarmManagedByValue,
		types.LabelSwarmService:   name,
		"app":                     name,
	}
	if stack, ok := spec.Labels["com.docker.stack.namespace"]; ok {
		baseLabels[types.LabelSwarmStack] = stack
	}
	for k, v := range spec.Labels {
		if clean, ok := sanitiseLabelValue(v); ok {
			baseLabels[k] = clean
		}
	}

	// --- command / args ---
	// Swarm Command = entrypoint override, Args = cmd override.
	var cmd, args []string
	if len(cs.Command) > 0 {
		cmd = cs.Command
	}
	if len(cs.Args) > 0 {
		args = cs.Args
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: a.namespace,
			Labels:    baseLabels,
			Annotations: map[string]string{
				types.AnnotationImageRef: cs.Image,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Strategy: updateStrategy,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":                     name,
						types.LabelManagedBy:      types.LabelManagedByValue,
						types.LabelSwarmManagedBy: types.LabelSwarmManagedByValue,
						types.LabelSwarmService:   name,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: restartPolicy,
					NodeSelector:  nodeSelector,
					Affinity: func() *corev1.Affinity {
						if nodeAffinity == nil {
							return nil
						}
						return &corev1.Affinity{NodeAffinity: nodeAffinity}
					}(),
					Volumes:       volumes,
					Containers: []corev1.Container{
						{
							Name:            name,
							Image:           cs.Image,
							Command:         cmd,
							Args:            args,
							Env:             envVars,
							Resources:       resourceReqs,
							VolumeMounts:    volumeMounts,
							ImagePullPolicy: corev1.PullAlways,
						},
					},
				},
			},
		},
	}

	// Stack label on pod template too, if present.
	if stack, ok := baseLabels[types.LabelSwarmStack]; ok {
		deployment.Spec.Template.Labels[types.LabelSwarmStack] = stack
	}

	created, err := a.client.AppsV1().Deployments(a.namespace).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			// docker stack deploy is idempotent — if the deployment already exists,
			// update it in place rather than failing. This matches real Swarm
			// behaviour where re-deploying a stack updates existing services.
			existing, getErr := a.client.AppsV1().Deployments(a.namespace).Get(ctx, name, metav1.GetOptions{})
			if getErr != nil {
				return nil, fmt.Errorf("service %q already exists and could not be retrieved: %w", name, getErr)
			}
			deployment.ResourceVersion = existing.ResourceVersion
			// Preserve the stable swarm service ID annotation from the existing deployment.
			if existing.Annotations[types.AnnotationSwarmServiceID] != "" {
				deployment.Annotations[types.AnnotationSwarmServiceID] = existing.Annotations[types.AnnotationSwarmServiceID]
			}
			updated, updateErr := a.client.AppsV1().Deployments(a.namespace).Update(ctx, deployment, metav1.UpdateOptions{})
			if updateErr != nil {
				return nil, fmt.Errorf("unable to update existing service %q: %w", name, updateErr)
			}
			serviceID := updated.Annotations[types.AnnotationSwarmServiceID]
			if serviceID == "" {
				serviceID = swarmID(string(updated.UID))
			}
			return map[string]any{
				"ID":       serviceID,
				"Warnings": warnings,
			}, nil
		}
		if errors.IsForbidden(err) {
			// Surface quota / RBAC rejections directly so the Docker CLI sees the
			// reason rather than hanging in the "preparing" task loop indefinitely.
			return nil, fmt.Errorf("deployment rejected by Kubernetes: %w", err)
		}
		return nil, fmt.Errorf("unable to create deployment for service %q: %w", name, err)
	}

	// Annotate the Deployment with its stable Swarm service ID.
	serviceID := swarmID(string(created.UID))
	created.Annotations[types.AnnotationSwarmServiceID] = serviceID
	_, _ = a.client.AppsV1().Deployments(a.namespace).Update(ctx, created, metav1.UpdateOptions{})

	// --- Kubernetes Services ---
	// Always create a ClusterIP Service so the service name resolves via
	// Kubernetes DNS within the namespace. This is what makes Docker short-name
	// DNS (e.g. "redis", "postgres") work between containers in a stack —
	// Kubernetes DNS resolves <service-name> to the ClusterIP from any pod in
	// the same namespace without needing a FQDN.
	// If ports are also published, a LoadBalancer Service is created separately
	// for external access (using a -lb suffix to avoid name collision).
	// Build ClusterIP ports from whichever port spec is available.
	// --- detect dnsrr / host-port mode ---
	// Triggered by either:
	//   EndpointSpec.Mode == "dnsrr"
	//   Any port with PublishMode == "host"
	// In this mode pods expose ports directly on the node via hostPort, and
	// the service endpoint returns the individual node IPs rather than a VIP.
	// No LoadBalancer Service is created — an external LB targets node IPs directly.
	isDNSRR := spec.EndpointSpec.Mode == "dnsrr"
	if !isDNSRR {
		for _, p := range spec.EndpointSpec.Ports {
			if strings.ToLower(p.PublishMode) == "host" {
				isDNSRR = true
				break
			}
		}
	}

	// Store endpoint mode as an annotation so inspect/endpoint functions can
	// read it later without re-parsing the spec.
	if isDNSRR {
		deployment.Annotations[types.AnnotationEndpointMode] = "dnsrr"
	}

	// For dnsrr/host-port mode, inject hostPort entries on the container spec.
	// This binds the container port directly on the node's network interface.
	if isDNSRR && len(spec.EndpointSpec.Ports) > 0 {
		var containerPorts []corev1.ContainerPort
		for _, p := range spec.EndpointSpec.Ports {
			if p.PublishedPort == 0 {
				continue
			}
			proto := corev1.ProtocolTCP
			if strings.ToUpper(p.Protocol) == "UDP" {
				proto = corev1.ProtocolUDP
			}
			containerPorts = append(containerPorts, corev1.ContainerPort{
				ContainerPort: int32(p.TargetPort),
				HostPort:      int32(p.PublishedPort),
				Protocol:      proto,
			})
		}
		if len(containerPorts) > 0 {
			deployment.Spec.Template.Spec.Containers[0].Ports = containerPorts
		}
	}

	// If no ports are declared at all we still create a headless-style ClusterIP
	// with no ports — enough to register the DNS name.
	var clusterIPPorts []corev1.ServicePort
	for i, p := range spec.EndpointSpec.Ports {
		proto := corev1.ProtocolTCP
		if strings.ToUpper(p.Protocol) == "UDP" {
			proto = corev1.ProtocolUDP
		}
		port := int32(p.TargetPort)
		if port == 0 {
			continue
		}
		clusterIPPorts = append(clusterIPPorts, corev1.ServicePort{
			Name:       fmt.Sprintf("port-%d", i),
			Protocol:   proto,
			Port:       port,
			TargetPort: intstr.FromInt(p.TargetPort),
		})
	}

	// Use a headless Service (clusterIP: None) when there are no ports.
	// Kubernetes rejects ClusterIP Services with an empty ports list, but
	// headless Services are allowed without ports and still register the DNS
	// name so short-name resolution (e.g. "redis") works from other pods.
	clusterSvcSpec := corev1.ServiceSpec{
		Selector: map[string]string{"app": name},
		Ports:    clusterIPPorts,
	}
	if len(clusterIPPorts) == 0 {
		clusterSvcSpec.ClusterIP = "None"
	} else {
		clusterSvcSpec.Type = corev1.ServiceTypeClusterIP
	}

	// Determine the DNS service name.
	// For stack services, Docker sends spec.Name as "<stack>_<service>" (e.g.
	// "example-app_redis"). We register DNS using just the bare service name
	// ("redis") so apps can connect using short names without stack prefixes.
	// For standalone services, the name is used as-is.
	dnsName := name
	if idx := strings.LastIndex(spec.Name, "_"); idx != -1 {
		if bare := sanitiseResourceName(spec.Name[idx+1:]); bare != "" {
			dnsName = bare
		}
	}

	clusterSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dnsName,
			Namespace: a.namespace,
			Labels:    baseLabels,
			Annotations: map[string]string{
				"d2k.portainer.io/dns-service":    "true",
				"d2k.portainer.io/dns-for-deploy": name,
			},
		},
		Spec: clusterSvcSpec,
	}
	if _, svcErr := a.client.CoreV1().Services(a.namespace).Create(ctx, clusterSvc, metav1.CreateOptions{}); svcErr != nil {
		if !errors.IsAlreadyExists(svcErr) {
			_ = a.client.AppsV1().Deployments(a.namespace).Delete(ctx, name, metav1.DeleteOptions{})
			return nil, fmt.Errorf("unable to create DNS service for %q: %w", name, svcErr)
		}
	}

	// Create a LoadBalancer Service for externally published ports — but only
	// in VIP mode. In dnsrr/host-port mode pods bind directly via hostPort and
	// an external LB targets node IPs; no LB Service is needed or appropriate.
	if !isDNSRR && len(spec.EndpointSpec.Ports) > 0 {
		var lbPorts []corev1.ServicePort
		for i, p := range spec.EndpointSpec.Ports {
			if p.PublishedPort == 0 {
				continue
			}
			proto := corev1.ProtocolTCP
			if strings.ToUpper(p.Protocol) == "UDP" {
				proto = corev1.ProtocolUDP
			}
			lbPorts = append(lbPorts, corev1.ServicePort{
				Name:       fmt.Sprintf("port-%d", i),
				Protocol:   proto,
				Port:       int32(p.PublishedPort),
				TargetPort: intstr.FromInt(p.TargetPort),
			})
		}
		if len(lbPorts) > 0 {
			lbSvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName(name) + "-lb",
					Namespace: a.namespace,
					Labels:    baseLabels,
				},
				Spec: corev1.ServiceSpec{
					Type:     corev1.ServiceTypeLoadBalancer,
					Selector: map[string]string{"app": name},
					Ports:    lbPorts,
				},
			}
			if _, svcErr := a.client.CoreV1().Services(a.namespace).Create(ctx, lbSvc, metav1.CreateOptions{}); svcErr != nil {
				warnings = append(warnings, fmt.Sprintf("d2k: unable to create LoadBalancer service for %q: %s", name, svcErr))
			}
		}
	}

	return map[string]any{
		"ID":       serviceID,
		"Warnings": warnings,
	}, nil
}

// SwarmListServices returns all d2k-managed Deployments as Swarm service objects.
func (a *KubernetesDockerAdapter) SwarmListServices(ctx context.Context) ([]map[string]any, error) {
	deps, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmManagedBy + "=" + types.LabelSwarmManagedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to list deployments: %w", err)
	}

	result := make([]map[string]any, 0, len(deps.Items))
	for _, d := range deps.Items {
		result = append(result, a.deploymentToSwarmService(ctx, d))
	}
	return result, nil
}

// SwarmInspectService returns a single Deployment as a Swarm service.
func (a *KubernetesDockerAdapter) SwarmInspectService(ctx context.Context, id string) (map[string]any, error) {
	deps, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmManagedBy + "=" + types.LabelSwarmManagedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to list deployments: %w", err)
	}
	for _, d := range deps.Items {
		if matchesServiceID(d, id) {
			return a.deploymentToSwarmService(ctx, d), nil
		}
	}
	return nil, fmt.Errorf("service %q not found", id)
}

// SwarmUpdateService handles scale / image / env updates via a full service spec replace.
// Docker CLI sends the full ServiceSpec on every update (same as kubectl apply).
// The version query param is advisory only - we don't enforce it but we don't reject it.
func (a *KubernetesDockerAdapter) SwarmUpdateService(ctx context.Context, id string, body io.Reader) error {
	var spec swarmServiceSpec
	if err := json.NewDecoder(body).Decode(&spec); err != nil {
		return fmt.Errorf("invalid service update spec: %w", err)
	}

	// Resolve the Deployment by swarm service ID or name.
	deps, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmManagedBy + "=" + types.LabelSwarmManagedByValue,
	})
	if err != nil {
		return fmt.Errorf("unable to list deployments: %w", err)
	}

	var target *appsv1.Deployment
	for i, d := range deps.Items {
		if matchesServiceID(d, id) {
			target = &deps.Items[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("service %q not found", id)
	}

	// Apply updates - only patch the fields the spec carries.
	cs := spec.TaskTemplate.ContainerSpec
	if cs.Image != "" {
		target.Spec.Template.Spec.Containers[0].Image = cs.Image
		target.Annotations[types.AnnotationImageRef] = cs.Image
	}

	if len(cs.Env) > 0 {
		var envVars []corev1.EnvVar
		for _, e := range cs.Env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				envVars = append(envVars, corev1.EnvVar{Name: parts[0], Value: parts[1]})
			}
		}
		target.Spec.Template.Spec.Containers[0].Env = envVars
	}

	if spec.Mode.Replicated != nil && spec.Mode.Replicated.Replicas > 0 {
		r := int32(spec.Mode.Replicated.Replicas)
		target.Spec.Replicas = &r
		// Cache desired replica count in annotation so ServiceInspect returns
		// the correct value immediately, before Kubernetes propagates the update.
		target.Annotations["d2k.portainer.io/desired-replicas"] = fmt.Sprintf("%d", r)
	}

	// Don't set any update annotations - let task polling drive convergence.
	// Kubernetes reconciles the scale immediately, so all pods will be
	// running by the time the CLI polls tasks. The progress loop exits
	// naturally once running == replicas.
	if target.Annotations == nil {
		target.Annotations = map[string]string{}
	}
	delete(target.Annotations, "d2k.portainer.io/update-in-progress")
	delete(target.Annotations, "d2k.portainer.io/update-requested")

	for attempt := 0; attempt < 5; attempt++ {
		_, err = a.client.AppsV1().Deployments(a.namespace).Update(ctx, target, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if !errors.IsConflict(err) {
			return fmt.Errorf("unable to update service %q: %w", id, err)
		}
		// Re-fetch on conflict and re-apply.
		fresh, getErr := a.client.AppsV1().Deployments(a.namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		target = fresh
	}
	return fmt.Errorf("unable to update service %q: too many conflicts", id)
}

// SwarmDeleteService removes the Deployment and any associated k8s Service.
func (a *KubernetesDockerAdapter) SwarmDeleteService(ctx context.Context, id string) error {
	deps, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmManagedBy + "=" + types.LabelSwarmManagedByValue,
	})
	if err != nil {
		return fmt.Errorf("unable to list deployments: %w", err)
	}
	for _, d := range deps.Items {
		if matchesServiceID(d, id) {
			// Delete the Deployment.
			if err := a.client.AppsV1().Deployments(a.namespace).Delete(ctx, d.Name, metav1.DeleteOptions{}); err != nil {
				return err
			}
			// Delete DNS service (bare name for stack services, full name for standalone).
			dnsName := d.Name
			if idx := strings.LastIndex(d.Name, "-"); idx != -1 {
				if bare := d.Name[idx+1:]; bare != "" {
					dnsName = bare
				}
			}
			_ = a.client.CoreV1().Services(a.namespace).Delete(ctx, dnsName, metav1.DeleteOptions{})
			_ = a.client.CoreV1().Services(a.namespace).Delete(ctx, serviceName(d.Name)+"-lb", metav1.DeleteOptions{})
			return nil
		}
	}
	return fmt.Errorf("service %q not found", id)
}

// SwarmServiceLogs fans out to all Pods for a service and multiplexes their log
// streams into a single response. Query params mirror docker service logs:
// follow, timestamps, tail.
func (a *KubernetesDockerAdapter) SwarmServiceLogs(ctx context.Context, w io.Writer, id string, query url.Values) error {
	// Resolve service ID ??? deployment name.
	deps, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmManagedBy + "=" + types.LabelSwarmManagedByValue,
	})
	if err != nil {
		return fmt.Errorf("unable to list deployments: %w", err)
	}

	var deploymentName string
	var svcID string
	for _, d := range deps.Items {
		if matchesServiceID(d, id) {
			deploymentName = d.Name
			svcID = d.Annotations[types.AnnotationSwarmServiceID]
			if svcID == "" {
				svcID = swarmID(string(d.UID))
			}
			break
		}
	}
	if deploymentName == "" {
		return fmt.Errorf("service %q not found", id)
	}

	follow := query.Get("follow") == "1" || query.Get("follow") == "true"
	timestamps := query.Get("timestamps") == "1" || query.Get("timestamps") == "true"
	tail := query.Get("tail")

	var tailLines *int64
	if tail != "" && tail != "all" {
		n := int64(0)
		if _, scanErr := fmt.Sscan(tail, &n); scanErr == nil && n > 0 {
			tailLines = &n
		}
	}

	// Find all Pods for this service.
	pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s",
			types.LabelSwarmService, deploymentName,
			types.LabelSwarmManagedBy, types.LabelSwarmManagedByValue,
		),
	})
	if err != nil {
		return fmt.Errorf("unable to list pods for service %q: %w", id, err)
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("no pods found for service %q", id)
	}

	// Fan out concurrently to all pods. Each line is prefixed with the Docker
	// swarm log details context so the CLI can parse task/node/service info.
	var mu sync.Mutex
	var wg sync.WaitGroup

	nodeSwarmIDs, _ := a.nodeNameToSwarmID(ctx)

	for i, pod := range pods.Items {
		podName := pod.Name
		podUID := string(pod.UID)
		slot := i + 1
		nodeSwarmID := nodeSwarmIDs[pod.Spec.NodeName]
		wg.Add(1)
		go func(name, uid, nodeID string, slotNum int) {
			defer wg.Done()
			logOpts := &corev1.PodLogOptions{
				Follow:     follow,
				Timestamps: timestamps,
				TailLines:  tailLines,
			}
			req := a.client.CoreV1().Pods(a.namespace).GetLogs(name, logOpts)
			stream, streamErr := req.Stream(ctx)
			if streamErr != nil {
				a.logger.Warnw("unable to stream logs for pod", "pod", name, "error", streamErr)
				return
			}
			defer stream.Close()

			// Close the stream if the context is cancelled (client disconnected).
			go func() {
				<-ctx.Done()
				stream.Close()
			}()

			// Build the Docker swarm details prefix the CLI requires.
			// Format: comma-separated url-escaped key=value pairs.
			// Parsed by logdetails.Parse which splits on comma not &.
			// Required: com.docker.swarm.node.id, com.docker.swarm.service.id,
			//           com.docker.swarm.task.id
			taskID := swarmID(uid)
			taskName := fmt.Sprintf("%s.%d.%s", deploymentName, slotNum, taskID)
			pairs := []string{
				url.QueryEscape("com.docker.swarm.node.id") + "=" + url.QueryEscape(nodeID),
				url.QueryEscape("com.docker.swarm.service.id") + "=" + url.QueryEscape(svcID),
				url.QueryEscape("com.docker.swarm.task.id") + "=" + url.QueryEscape(taskID),
				url.QueryEscape("com.docker.swarm.task.name") + "=" + url.QueryEscape(taskName),
			}
			prefix := strings.Join(pairs, ",") + " "

			reader := bufio.NewReader(stream)
			for {
				if ctx.Err() != nil {
					return
				}
				text, err := reader.ReadString('\n')
				if len(text) > 0 {
					// Strip trailing newline - we add it back with prefix
					text = strings.TrimRight(text, "\n")
					line := prefix + text + "\n"
					mu.Lock()
					_, _ = io.WriteString(w, line)
					mu.Unlock()
				}
				if err != nil {
					return
				}
			}
		}(podName, podUID, nodeSwarmID, slot)
	}

	wg.Wait()
	return nil
}

// SwarmListTasks returns Swarm task objects for all d2k-managed services.
// Tasks are synthesised from Deployments + Pods: if a pod exists it is used
// directly; if not (e.g. pod still scheduling) a synthetic task is derived
// from the Deployment so the CLI sees progress immediately.
// serviceFilter, if non-empty, scopes results to a single service.
func (a *KubernetesDockerAdapter) SwarmListTasks(ctx context.Context, serviceFilter, stackFilter string) ([]map[string]any, error) {
	// Build a node-name -> swarm ID map so task NodeID values match what
	// GET /nodes returns. The CLI filters out tasks whose NodeID is not in
	// the active node list, so this must match exactly.
	nodeSwarmIDs, _ := a.nodeNameToSwarmID(ctx)

	// Always work from Deployments as the source of truth for services.
	depSelector := types.LabelSwarmManagedBy + "=" + types.LabelSwarmManagedByValue
	deps, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{LabelSelector: depSelector})
	if err != nil {
		return nil, fmt.Errorf("unable to list deployments: %w", err)
	}

	result := make([]map[string]any, 0)

	for _, dep := range deps.Items {
		// Resolve stable service ID.
		svcID := dep.Annotations[types.AnnotationSwarmServiceID]
		if svcID == "" {
			svcID = swarmID(string(dep.UID))
		}

		// Apply service filter if present - match by swarm ID or deployment name.
		if serviceFilter != "" && serviceFilter != svcID && serviceFilter != dep.Name {
			continue
		}

		// Apply stack filter if present - match by stack label.
		if stackFilter != "" && dep.Labels[types.LabelSwarmStack] != stackFilter {
			continue
		}

		// Find pods for this deployment.
		pods, podErr := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s,%s=%s", dep.Name, types.LabelSwarmManagedBy, types.LabelSwarmManagedByValue),
		})

		// Determine desired replica count.
		desired := dep.Status.Replicas
		if dep.Spec.Replicas != nil && *dep.Spec.Replicas > desired {
			desired = *dep.Spec.Replicas
		}

		// Pick a fallback node ID for synthetic tasks.
		// Prefer a known node from the node list; fall back to the node ID
		// of any already-running pod so synthetic tasks pass the activeNodes
		// filter in the CLI even if the node list call failed.
		syntheticNodeID := ""
		for _, id := range nodeSwarmIDs {
			syntheticNodeID = id
			break
		}
		if syntheticNodeID == "" {
			for _, p := range pods.Items {
				if p.Spec.NodeName != "" {
					syntheticNodeID = swarmID(string(p.Spec.NodeName))
					break
				}
			}
		}

		// Build a slot -> pod map from real pods.
		// Sort by creation timestamp so slot assignment is stable across polls.
		// Oldest pod = slot 1, next oldest = slot 2, etc.
		podsBySlot := map[int32]corev1.Pod{}
		if podErr == nil && len(pods.Items) > 0 {
			sorted := make([]corev1.Pod, len(pods.Items))
			copy(sorted, pods.Items)
			sort.Slice(sorted, func(i, j int) bool {
				return sorted[i].CreationTimestamp.Before(&sorted[j].CreationTimestamp)
			})
			for i, p := range sorted {
				podsBySlot[int32(i+1)] = p
			}
		}

		// Return one task per desired slot: real pod if available, synthetic otherwise.
		for slot := int32(1); slot <= desired; slot++ {
			if p, ok := podsBySlot[slot]; ok {
				nodeID := nodeSwarmIDs[p.Spec.NodeName]
				result = append(result, kubePodToSwarmTask(p, svcID, nodeID, int(slot)))
			} else {
				// Pod not yet scheduled - synthesise a preparing task.
				result = append(result, map[string]any{
					"ID":        swarmID(fmt.Sprintf("%s-task-%d", string(dep.UID), slot)),
					"Version":   map[string]any{"Index": uint64(1)},
					"CreatedAt": dep.CreationTimestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"),
					"UpdatedAt": dep.CreationTimestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"),
					"Spec": map[string]any{
						"ContainerSpec": map[string]any{
							"Image": dep.Annotations[types.AnnotationImageRef],
						},
					},
					"ServiceID":    svcID,
					"Slot":         int(slot),
					"NodeID":       syntheticNodeID,
					"Status": map[string]any{
						"State":     "preparing",
						"Message":   "",
						"Timestamp": dep.CreationTimestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"),
					},
					"DesiredState": "running",
				})
			}
		}
	}

	return result, nil
}

// nodeNameToSwarmID returns a map of Kubernetes node name to swarm-format ID.
// Used to ensure task NodeID values match the IDs returned by GET /nodes.
func (a *KubernetesDockerAdapter) nodeNameToSwarmID(ctx context.Context) (map[string]string, error) {
	nodeList, err := a.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(nodeList.Items))
	for _, n := range nodeList.Items {
		m[n.Name] = swarmID(string(n.UID))
	}
	return m, nil
}


// SwarmInspectTask returns a single Pod as a Swarm task.
func (a *KubernetesDockerAdapter) SwarmInspectTask(ctx context.Context, id string) (map[string]any, error) {
	pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmManagedBy + "=" + types.LabelSwarmManagedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to list pods: %w", err)
	}
	for _, p := range pods.Items {
		if swarmID(string(p.UID)) == id || string(p.UID) == id || p.Name == id {
			return kubePodToSwarmTask(p, "", "", 0), nil
		}
	}
	return nil, fmt.Errorf("task %q not found", id)
}

// SwarmCreateSecret creates a Kubernetes Secret tagged for d2k.
func (a *KubernetesDockerAdapter) SwarmCreateSecret(ctx context.Context, body io.Reader) (map[string]any, error) {
	var req struct {
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels"`
		Data   string            `json:"Data"` // base64-encoded
	}
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		return nil, fmt.Errorf("invalid secret request: %w", err)
	}

	// Docker sends secret Data as a base64-encoded string — decode before storing
	// so the raw value is available when mounted into containers.
	decodedData, decErr := base64.StdEncoding.DecodeString(req.Data)
	if decErr != nil {
		// Fall back to raw bytes if decoding fails (e.g. already raw).
		decodedData = []byte(req.Data)
	}

	// Kubernetes secret names must be RFC 1123 subdomains: lowercase alphanumeric,
	// hyphens and dots only. Swarm allows underscores (e.g. "example-app_postgres_pw")
	// so sanitise before creating the resource.
	k8sName := sanitiseResourceName(req.Name)
	if k8sName == "" {
		return nil, fmt.Errorf("secret Name %q is invalid after sanitisation", req.Name)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      k8sName,
			Namespace: a.namespace,
			Labels:    swarmLabels(req.Labels),
		},
		Data: map[string][]byte{k8sName: decodedData},
	}

	// Store the original Swarm name as an annotation so list/inspect can
	// return the underscore form that the CLI and compose files expect.
	secret.ObjectMeta.Annotations = map[string]string{
		"d2k.portainer.io/swarm-name": req.Name,
	}

	created, err := a.client.CoreV1().Secrets(a.namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			// Be idempotent only for stack deploy, which re-creates secrets on
			// every deploy. Stack secrets always carry com.docker.stack.namespace.
			// A direct `docker secret create` should return an error as Swarm does.
			if _, isStackSecret := req.Labels["com.docker.stack.namespace"]; isStackSecret {
				existing, getErr := a.client.CoreV1().Secrets(a.namespace).Get(ctx, k8sName, metav1.GetOptions{})
				if getErr != nil {
					return nil, fmt.Errorf("secret %q already exists", k8sName)
				}
				return map[string]any{"ID": swarmID(string(existing.UID))}, nil
			}
			return nil, fmt.Errorf("secret %q already exists", req.Name)
		}
		return nil, fmt.Errorf("unable to create secret: %w", err)
	}
	return map[string]any{"ID": swarmID(string(created.UID))}, nil
}

// SwarmListSecrets returns all d2k-managed Secrets.
func (a *KubernetesDockerAdapter) SwarmListSecrets(ctx context.Context) ([]map[string]any, error) {
	secrets, err := a.client.CoreV1().Secrets(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmManagedBy + "=" + types.LabelSwarmManagedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to list secrets: %w", err)
	}

	result := make([]map[string]any, 0, len(secrets.Items))
	for _, s := range secrets.Items {
		result = append(result, kubeSecretToSwarm(s))
	}
	return result, nil
}

// SwarmInspectSecret returns a single Secret.
func (a *KubernetesDockerAdapter) SwarmInspectSecret(ctx context.Context, id string) (map[string]any, error) {
	secrets, err := a.client.CoreV1().Secrets(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmManagedBy + "=" + types.LabelSwarmManagedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to list secrets: %w", err)
	}
	for _, s := range secrets.Items {
		if swarmID(string(s.UID)) == id || s.Name == id ||
			s.Annotations["d2k.portainer.io/swarm-name"] == id ||
			sanitiseResourceName(id) == s.Name {
			return kubeSecretToSwarm(s), nil
		}
	}
	return nil, fmt.Errorf("secret %q not found", id)
}

// SwarmDeleteSecret deletes a Secret.
func (a *KubernetesDockerAdapter) SwarmDeleteSecret(ctx context.Context, id string) error {
	secrets, err := a.client.CoreV1().Secrets(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmManagedBy + "=" + types.LabelSwarmManagedByValue,
	})
	if err != nil {
		return fmt.Errorf("unable to list secrets: %w", err)
	}
	for _, s := range secrets.Items {
		if swarmID(string(s.UID)) == id || s.Name == id ||
			s.Annotations["d2k.portainer.io/swarm-name"] == id ||
			sanitiseResourceName(id) == s.Name {
			return a.client.CoreV1().Secrets(a.namespace).Delete(ctx, s.Name, metav1.DeleteOptions{})
		}
	}
	return fmt.Errorf("secret %q not found", id)
}

// SwarmCreateConfig creates a Kubernetes ConfigMap tagged for d2k.
func (a *KubernetesDockerAdapter) SwarmCreateConfig(ctx context.Context, body io.Reader) (map[string]any, error) {
	var req struct {
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels"`
		Data   string            `json:"Data"`
	}
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		return nil, fmt.Errorf("invalid config request: %w", err)
	}

	// Kubernetes ConfigMap names must be RFC 1123 subdomains. Sanitise for the
	// same reason as secrets — Swarm allows underscores, Kubernetes does not.
	k8sName := sanitiseResourceName(req.Name)
	if k8sName == "" {
		return nil, fmt.Errorf("config Name %q is invalid after sanitisation", req.Name)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      k8sName,
			Namespace: a.namespace,
			Labels:    swarmLabels(req.Labels),
		},
		Data: map[string]string{k8sName: req.Data},
	}

	cm.ObjectMeta.Annotations = map[string]string{
		"d2k.portainer.io/swarm-name": req.Name,
	}

	created, err := a.client.CoreV1().ConfigMaps(a.namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			if _, isStackConfig := req.Labels["com.docker.stack.namespace"]; isStackConfig {
				existing, getErr := a.client.CoreV1().ConfigMaps(a.namespace).Get(ctx, k8sName, metav1.GetOptions{})
				if getErr != nil {
					return nil, fmt.Errorf("config %q already exists", k8sName)
				}
				return map[string]any{"ID": swarmID(string(existing.UID))}, nil
			}
			return nil, fmt.Errorf("config %q already exists", req.Name)
		}
		return nil, fmt.Errorf("unable to create config: %w", err)
	}
	return map[string]any{"ID": swarmID(string(created.UID))}, nil
}

// SwarmListConfigs returns all d2k-managed ConfigMaps.
func (a *KubernetesDockerAdapter) SwarmListConfigs(ctx context.Context) ([]map[string]any, error) {
	cms, err := a.client.CoreV1().ConfigMaps(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmManagedBy + "=" + types.LabelSwarmManagedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to list configs: %w", err)
	}

	result := make([]map[string]any, 0, len(cms.Items))
	for _, c := range cms.Items {
		// Exclude internal d2k system ConfigMaps from the Swarm config list.
		if c.Name == types.ConfigMapSwarmIdentity {
			continue
		}
		result = append(result, kubeConfigMapToSwarm(c))
	}
	return result, nil
}

// SwarmInspectConfig returns a single ConfigMap.
func (a *KubernetesDockerAdapter) SwarmInspectConfig(ctx context.Context, id string) (map[string]any, error) {
	cms, err := a.client.CoreV1().ConfigMaps(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmManagedBy + "=" + types.LabelSwarmManagedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to list configs: %w", err)
	}
	for _, c := range cms.Items {
		if swarmID(string(c.UID)) == id || c.Name == id ||
			c.Annotations["d2k.portainer.io/swarm-name"] == id ||
			sanitiseResourceName(id) == c.Name {
			return kubeConfigMapToSwarm(c), nil
		}
	}
	return nil, fmt.Errorf("config %q not found", id)
}

// SwarmDeleteConfig deletes a ConfigMap.
func (a *KubernetesDockerAdapter) SwarmDeleteConfig(ctx context.Context, id string) error {
	cms, err := a.client.CoreV1().ConfigMaps(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmManagedBy + "=" + types.LabelSwarmManagedByValue,
	})
	if err != nil {
		return fmt.Errorf("unable to list configs: %w", err)
	}
	for _, c := range cms.Items {
		if swarmID(string(c.UID)) == id || c.Name == id ||
			c.Annotations["d2k.portainer.io/swarm-name"] == id ||
			sanitiseResourceName(id) == c.Name {
			return a.client.CoreV1().ConfigMaps(a.namespace).Delete(ctx, c.Name, metav1.DeleteOptions{})
		}
	}
	return fmt.Errorf("config %q not found", id)
}

// SwarmListStacks returns all stacks derived from d2k-managed Deployments.
// A stack is a group of services sharing the same LabelSwarmStack value.
// docker stack ls synthesises this from service labels; Portainer may call
// it directly via a non-standard endpoint.
func (a *KubernetesDockerAdapter) SwarmListStacks(ctx context.Context) ([]map[string]any, error) {
	deps, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmManagedBy + "=" + types.LabelSwarmManagedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to list deployments: %w", err)
	}

	// Aggregate services by stack name.
	// Only include services that have a stack label - standalone services
	// without com.docker.stack.namespace are excluded to prevent
	// "cannot get label" errors in the CLI.
	stacks := map[string]int{}
	for _, d := range deps.Items {
		stack := d.Labels[types.LabelSwarmStack]
		if stack == "" {
			continue
		}
		stacks[stack]++
	}

	result := make([]map[string]any, 0, len(stacks))
	for name, count := range stacks {
		result = append(result, map[string]any{
			"Name":         name,
			"Services":     count,
			"Orchestrator": "Swarm",
		})
	}
	return result, nil
}

// SwarmDeleteStack removes all services (Deployments + k8s Services) belonging
// to a stack, plus any secrets and configs labelled with that stack name.
func (a *KubernetesDockerAdapter) SwarmDeleteStack(ctx context.Context, stackName string) error {
	// Delete Deployments.
	deps, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmStack + "=" + stackName,
	})
	if err != nil {
		return fmt.Errorf("unable to list deployments for stack %q: %w", stackName, err)
	}
	for _, d := range deps.Items {
		_ = a.client.AppsV1().Deployments(a.namespace).Delete(ctx, d.Name, metav1.DeleteOptions{})
		_ = a.client.CoreV1().Services(a.namespace).Delete(ctx, serviceName(d.Name)+"-lb", metav1.DeleteOptions{})
		// DNS service is registered under the bare name for stack services.
		dnsName := d.Name
		if idx := strings.LastIndex(d.Name, "-"); idx != -1 {
			if bare := d.Name[idx+1:]; bare != "" {
				dnsName = bare
			}
		}
		_ = a.client.CoreV1().Services(a.namespace).Delete(ctx, dnsName, metav1.DeleteOptions{})
	}

	// Delete Secrets labelled with this stack.
	secrets, _ := a.client.CoreV1().Secrets(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmStack + "=" + stackName,
	})
	if secrets != nil {
		for _, s := range secrets.Items {
			_ = a.client.CoreV1().Secrets(a.namespace).Delete(ctx, s.Name, metav1.DeleteOptions{})
		}
	}

	// Delete ConfigMaps labelled with this stack.
	cms, _ := a.client.CoreV1().ConfigMaps(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmStack + "=" + stackName,
	})
	if cms != nil {
		for _, c := range cms.Items {
			_ = a.client.CoreV1().ConfigMaps(a.namespace).Delete(ctx, c.Name, metav1.DeleteOptions{})
		}
	}

	// Delete in-memory networks belonging to this stack.
	// Stack networks follow the "<stack>_<network>" naming convention.
	// We match by the com.docker.compose.project label stored at create time.
	a.networksMu.Lock()
	for netName, net := range a.networks {
		if net.Labels["com.docker.compose.project"] == stackName {
			delete(a.networks, netName)
		}
	}
	a.networksMu.Unlock()

	return nil
}

func kubeNodeToSwarm(n corev1.Node, apiServerHost string) map[string]any {
	role := "worker"
	_, isControlPlane := n.Labels["node-role.kubernetes.io/control-plane"]
	_, isMaster := n.Labels["node-role.kubernetes.io/master"] // older clusters
	if isControlPlane || isMaster {
		role = "manager"
	}

	availability := "active"
	if n.Spec.Unschedulable {
		availability = "drain"
	}

	state := "ready"
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue {
			state = "down"
		}
	}

	nodeID := swarmID(string(n.UID))

	return map[string]any{
		"ID": nodeID,
		"Version": map[string]any{"Index": uint64(1)},
		"CreatedAt": n.CreationTimestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		"UpdatedAt": n.CreationTimestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		"Spec": map[string]any{
			"Labels":       n.Labels,
			"Role":         role,
			"Availability": availability,
		},
		"Description": map[string]any{
			"Hostname": n.Name,
			"Platform": map[string]any{
				"Architecture": n.Status.NodeInfo.Architecture,
				"OS":           n.Status.NodeInfo.OperatingSystem,
			},
			"Resources": map[string]any{
				"NanoCPUs":    n.Status.Capacity.Cpu().MilliValue() * 1e6,
				"MemoryBytes": n.Status.Capacity.Memory().Value(),
			},
			"Engine": map[string]any{
				"EngineVersion": "d2k/" + n.Status.NodeInfo.KubeletVersion,
				// Plugins must be a flat array of {Type, Name} objects.
				// Portainer's NodeDetailsViewController calls transformPlugins()
				// which filters this array by Type — if missing, .filter() throws
				// "can't access property filter, t is undefined".
				"Plugins": []map[string]any{
					{"Type": "Network", "Name": "bridge"},
					{"Type": "Network", "Name": "host"},
					{"Type": "Network", "Name": "null"},
					{"Type": "Log",     "Name": "json-file"},
				},
			},
		},
		"Status": map[string]any{
			"State": state,
			"Addr":  nodeAddress(n),
		},
		"ManagerStatus": managerStatus(n, role, apiServerHost),
	}
}

func nodeAddress(n corev1.Node) string {
	for _, addr := range n.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}
	}
	return ""
}

func managerStatus(n corev1.Node, role, apiServerHost string) any {
	if role != "manager" {
		return nil
	}
	// A node is the leader if its internal IP (or external IP, as fallback)
	// matches the host d2k is using to reach the Kubernetes API server.
	// In a single control-plane cluster this is always true for the one manager.
	// In a multi-control-plane cluster only the node actually serving the API
	// gets Leader=true; the others are reachable managers but not leader.
	leader := false
	if apiServerHost != "" {
		for _, addr := range n.Status.Addresses {
			if (addr.Type == corev1.NodeInternalIP || addr.Type == corev1.NodeExternalIP) &&
				addr.Address == apiServerHost {
				leader = true
				break
			}
		}
		// Also match by hostname in case the API server URL uses a DNS name.
		if !leader && n.Name == apiServerHost {
			leader = true
		}
	}
	return map[string]any{
		"Leader":       leader,
		"Reachability": "reachable",
		"Addr":         nodeAddress(n),
	}
}

// serviceSpecLabels returns the Docker-facing labels for a service spec.
// It translates internal d2k label keys to Docker Swarm label keys so the
// CLI can find stack services by com.docker.stack.namespace.
func serviceSpecLabels(depLabels map[string]string) map[string]string {
	result := make(map[string]string, len(depLabels))
	for k, v := range depLabels {
		result[k] = v
	}
	// Expose stack identity under the Docker label key the CLI expects.
	if stack, ok := depLabels[types.LabelSwarmStack]; ok && stack != "" {
		result["com.docker.stack.namespace"] = stack
	}
	return result
}

// deploymentToSwarmService converts a Deployment to a Swarm service map,
// looking up the associated LoadBalancer Service to populate Endpoint.Ports
// with the external IP and published port mappings.
func (a *KubernetesDockerAdapter) deploymentToSwarmService(ctx context.Context, d appsv1.Deployment) map[string]any {
	serviceID := d.Annotations[types.AnnotationSwarmServiceID]
	if serviceID == "" {
		serviceID = swarmID(string(d.UID))
	}

	image := d.Annotations[types.AnnotationImageRef]
	var envSlice []string
	if len(d.Spec.Template.Spec.Containers) > 0 {
		c := d.Spec.Template.Spec.Containers[0]
		if image == "" {
			image = c.Image
		}
		for _, e := range c.Env {
			envSlice = append(envSlice, e.Name+"="+e.Value)
		}
	}

	replicas := int64(1)
	if d.Spec.Replicas != nil {
		replicas = int64(*d.Spec.Replicas)
	}
	// If a scale/update annotation is present, use it as the authoritative
	// replica count. This ensures ServiceInspect returns the correct value
	// immediately after an update, before Kubernetes propagates the change.
	if v := d.Annotations["d2k.portainer.io/desired-replicas"]; v != "" {
		var parsed int64
		if _, err := fmt.Sscan(v, &parsed); err == nil && parsed > replicas {
			replicas = parsed
		}
	}

	runningTasks := int64(d.Status.ReadyReplicas)

	// Use the last ready condition time as UpdatedAt so the CLI sees the
	// service as converged (UpdatedAt > CreatedAt).
	updatedAt := d.CreationTimestamp.UTC()
	for _, c := range d.Status.Conditions {
		if c.Type == "Available" && c.Status == "True" {
			if c.LastUpdateTime.After(updatedAt) {
				updatedAt = c.LastUpdateTime.UTC()
			}
		}
	}

	// UpdateStatus is only set during/after a service update (not on initial
	// creation). If present and "completed", the CLI progress loop exits
	// immediately on first poll - so we must not set it on new services.
	//
	// Rules:
	//   - update-in-progress annotation set -> "updating" (blocks CLI loop)
	//   - replicas not yet ready, no annotation -> nil (CLI polls tasks)
	//   - replicas ready AND annotation was previously set -> "completed"
	// UpdateStatus is only set when an explicit service update has been
	// requested (via SwarmUpdateService). New services always return nil
	// so the CLI drives convergence via task polling instead.
	// The "d2k.portainer.io/update-requested" annotation is set by
	// SwarmUpdateService and never by SwarmCreateService.
	var updateStatus map[string]any
	updateInProgress := d.Annotations["d2k.portainer.io/update-in-progress"] == "true"
	updateRequested := d.Annotations["d2k.portainer.io/update-requested"] == "true"
	allReady := d.Status.ReadyReplicas == d.Status.Replicas && d.Status.Replicas > 0

	if updateInProgress {
		updateStatus = map[string]any{
			"State":   "updating",
			"Message": "",
		}
	} else if updateRequested && allReady {
		// Update was explicitly requested and all replicas are ready.
		updateStatus = map[string]any{
			"State":       "completed",
			"CompletedAt": updatedAt.Format("2006-01-02T15:04:05.000000000Z"),
			"Message":     "",
		}
	}
	// else nil: new service or no update in flight - task polling drives convergence

	// Version.Index uses the deployment generation so it increments on each
	// update. The Docker CLI sends this back as a version check on updates.
	versionIndex := uint64(1)
	if d.Generation > 0 {
		versionIndex = uint64(d.Generation)
	}

	return map[string]any{
		"ID": serviceID,
		"Version": map[string]any{"Index": versionIndex},
		"CreatedAt": d.CreationTimestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		"UpdatedAt": updatedAt.Format("2006-01-02T15:04:05.000000000Z"),
		"Spec": map[string]any{
			"Name":   d.Labels[types.LabelSwarmService],
			"Labels": serviceSpecLabels(d.Labels),
			"TaskTemplate": map[string]any{
				"ContainerSpec": map[string]any{
					"Image": image,
					"Env":   envSlice,
				},
				// Networks: Portainer reads TaskTemplate.Networks to populate the
				// "Networks" panel in the service detail view.
				"Networks": []map[string]any{
					{"Target": networkIDForName(d.Name, a.namespace), "Aliases": []string{d.Name}},
				},
			},
			"Mode": map[string]any{
				"Replicated": map[string]any{
					"Replicas": replicas,
				},
			},
			// Networks at the service spec level — also read by some Portainer versions.
			"Networks": []map[string]any{
				{"Target": networkIDForName(d.Name, a.namespace), "Aliases": []string{d.Name}},
			},
			// EndpointSpec.Ports: Portainer service detail reads this to render
			// the "Published ports" panel. Populated from the LB Service if present.
			"EndpointSpec": a.swarmServiceEndpointSpec(ctx, d.Name),
		},
		"ServiceStatus": map[string]any{
			"RunningTasks":   runningTasks,
			"DesiredTasks":   replicas,
			"CompletedTasks": 0,
		},
		"Endpoint": func() map[string]any {
			if d.Annotations[types.AnnotationEndpointMode] == "dnsrr" {
				return a.swarmServiceEndpointDNSRR(ctx, d)
			}
			return a.swarmServiceEndpoint(ctx, d.Name)
		}(),
		"UpdateStatus": updateStatus, // nil on new services - CLI polls tasks instead
	}
}

// swarmServiceEndpoint looks up the LoadBalancer Service for a swarm service
// (named <service>-lb) and returns a Docker Endpoint map populated with the
// external IP and published port mappings. Falls back to empty arrays when no
// LB Service exists or the LB IP has not yet been assigned.
func (a *KubernetesDockerAdapter) swarmServiceEndpoint(ctx context.Context, name string) map[string]any {
	lbName := serviceName(name) + "-lb"
	svc, err := a.client.CoreV1().Services(a.namespace).Get(ctx, lbName, metav1.GetOptions{})
	if err != nil {
		// No LB service — return empty endpoint.
		return map[string]any{
			"Spec":       map[string]any{},
			"Ports":      []any{},
			"VirtualIPs": []any{},
		}
	}

	// Extract the external IP assigned by the LB controller.
	externalIP := ""
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			externalIP = ing.IP
			break
		}
		if ing.Hostname != "" {
			externalIP = ing.Hostname
			break
		}
	}

	// Build the Ports list in Docker Swarm wire format.
	var ports []any
	for _, p := range svc.Spec.Ports {
		proto := "tcp"
		if p.Protocol == corev1.ProtocolUDP {
			proto = "udp"
		}
		entry := map[string]any{
			"Protocol":      proto,
			"TargetPort":    int(p.TargetPort.IntVal),
			"PublishedPort": int(p.Port),
			"PublishMode":   "ingress",
		}
		ports = append(ports, entry)
	}
	if ports == nil {
		ports = []any{}
	}

	// VirtualIPs: Portainer reads Endpoint.VirtualIPs[].Addr to display the
	// service IP in the services list. Use the external LB IP if available.
	virtualIPs := []any{}
	if externalIP != "" {
		// Docker CLI parses VirtualIPs[].Addr with netip.ParsePrefix so it
		// must be CIDR notation. Use /32 for IPv4, /128 for IPv6.
		cidr := externalIP + "/32"
		if strings.Contains(externalIP, ":") {
			cidr = externalIP + "/128"
		}
		virtualIPs = []any{
			map[string]any{
				// NetworkID must match Spec.Networks[].Target so Portainer can
				// associate the IP with the namespace network name.
				// NetworkID matches Spec.Networks[].Target so Portainer associates the IP
				// with the service network in the detail panel.
				"NetworkID": networkIDForName(name, a.namespace),
				"Addr":      cidr,
			},
		}
	}

	return map[string]any{
		// Spec.Ports: Portainer service detail panel reads EndpointSpec.Ports
		// to populate the "Published ports" section.
		"Spec": map[string]any{
			"Mode":  "vip",
			"Ports": ports,
		},
		"Ports":      ports,
		"VirtualIPs": virtualIPs,
	}
}


// swarmServiceEndpointDNSRR builds the Endpoint response for dnsrr/host-port
// services. Returns the individual node IPs where pods are running, with no
// VirtualIPs — matching Docker Swarm dnsrr behaviour where an external LB
// targets node IPs directly rather than a routing mesh VIP.
func (a *KubernetesDockerAdapter) swarmServiceEndpointDNSRR(ctx context.Context, d appsv1.Deployment) map[string]any {
	// Build published ports from hostPort entries on the container spec.
	var ports []any
	if len(d.Spec.Template.Spec.Containers) > 0 {
		for _, cp := range d.Spec.Template.Spec.Containers[0].Ports {
			proto := "tcp"
			if cp.Protocol == corev1.ProtocolUDP {
				proto = "udp"
			}
			ports = append(ports, map[string]any{
				"Protocol":      proto,
				"TargetPort":    int(cp.ContainerPort),
				"PublishedPort": int(cp.HostPort),
				"PublishMode":   "host",
			})
		}
	}
	if ports == nil {
		ports = []any{}
	}

	// Look up pods to get the node IPs where this service is actually running.
	// These are the IPs an external LB should target.
	pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s,%s=%s", d.Name, types.LabelSwarmManagedBy, types.LabelSwarmManagedByValue),
	})

	// Build per-node IP list — deduplicated since multiple pods may run on
	// the same node (though hostPort prevents that in practice).
	seen := map[string]bool{}
	var nodeIPs []any
	if err == nil {
		for _, p := range pods.Items {
			if p.Status.Phase != corev1.PodRunning {
				continue
			}
			nodeIP := ""
			for _, addr := range []corev1.PodHostIP{{IP: p.Status.HostIP}} {
				if addr.IP != "" {
					nodeIP = addr.IP
					break
				}
			}
			if nodeIP == "" {
				nodeIP = p.Status.HostIP
			}
			if nodeIP == "" || seen[nodeIP] {
				continue
			}
			seen[nodeIP] = true
			// Return as CIDR notation to match VirtualIPs[].Addr format.
			cidr := nodeIP + "/32"
			if strings.Contains(nodeIP, ":") {
				cidr = nodeIP + "/128"
			}
			nodeIPs = append(nodeIPs, map[string]any{
				"NetworkID": networkIDForName(d.Name, a.namespace),
				"Addr":      cidr,
			})
		}
	}
	if nodeIPs == nil {
		nodeIPs = []any{}
	}

	return map[string]any{
		"Spec": map[string]any{
			"Mode":  "dnsrr",
			"Ports": ports,
		},
		"Ports":      ports,
		"VirtualIPs": nodeIPs,
	}
}

// swarmServiceEndpointSpec returns the EndpointSpec block for the Spec field
// of a service inspect response. Portainer reads Spec.EndpointSpec.Ports to
// render the "Published ports" panel in the service detail view.
func (a *KubernetesDockerAdapter) swarmServiceEndpointSpec(ctx context.Context, name string) map[string]any {
	// Check if this is a dnsrr service by looking up the deployment annotation.
	deps, _ := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + name,
	})
	if deps != nil {
		for _, d := range deps.Items {
			if d.Annotations[types.AnnotationEndpointMode] == "dnsrr" {
				// Build ports from hostPort entries on the container spec.
				var ports []any
				if len(d.Spec.Template.Spec.Containers) > 0 {
					for _, cp := range d.Spec.Template.Spec.Containers[0].Ports {
						proto := "tcp"
						if cp.Protocol == corev1.ProtocolUDP {
							proto = "udp"
						}
						ports = append(ports, map[string]any{
							"Protocol":      proto,
							"TargetPort":    int(cp.ContainerPort),
							"PublishedPort": int(cp.HostPort),
							"PublishMode":   "host",
						})
					}
				}
				if ports == nil {
					ports = []any{}
				}
				return map[string]any{"Mode": "dnsrr", "Ports": ports}
			}
		}
	}

	lbName := serviceName(name) + "-lb"
	svc, err := a.client.CoreV1().Services(a.namespace).Get(ctx, lbName, metav1.GetOptions{})
	if err != nil {
		return map[string]any{"Mode": "vip", "Ports": []any{}}
	}
	var ports []any
	for _, p := range svc.Spec.Ports {
		proto := "tcp"
		if p.Protocol == corev1.ProtocolUDP {
			proto = "udp"
		}
		ports = append(ports, map[string]any{
			"Protocol":      proto,
			"TargetPort":    int(p.TargetPort.IntVal),
			"PublishedPort": int(p.Port),
			"PublishMode":   "ingress",
		})
	}
	if ports == nil {
		ports = []any{}
	}
	return map[string]any{"Mode": "vip", "Ports": ports}
}


func kubePodToSwarmTask(p corev1.Pod, serviceID string, nodeSwarmID string, slot int) map[string]any {
	// Map pod phase + container readiness to a Swarm task state.
	// Only report "running" when the pod is Running AND at least one
	// container is ready - otherwise the CLI progress loop won't advance.
	state := "preparing"
	switch p.Status.Phase {
	case corev1.PodRunning:
		// If ContainerStatuses is empty the pod just started and status
		// hasn't populated yet - treat as running so the CLI doesn't stall.
		if len(p.Status.ContainerStatuses) == 0 {
			state = "running"
		} else {
			ready := false
			for _, cs := range p.Status.ContainerStatuses {
				if cs.Ready {
					ready = true
					break
				}
			}
			if ready {
				state = "running"
			} else {
				state = "starting"
			}
		}
	case corev1.PodFailed:
		state = "failed"
	case corev1.PodSucceeded:
		state = "complete"
	}

	// ContainerStatus for docker service ps error column.
	containerStatus := map[string]any{
		"ContainerID": "",
		"PID":         0,
		"ExitCode":    0,
	}
	statusErr := p.Status.Message
	if len(p.Status.ContainerStatuses) > 0 {
		cs := p.Status.ContainerStatuses[0]
		containerStatus["ContainerID"] = cs.ContainerID
		if cs.State.Terminated != nil {
			containerStatus["ExitCode"] = cs.State.Terminated.ExitCode
			statusErr = cs.State.Terminated.Message
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Message != "" {
			statusErr = cs.State.Waiting.Message
		}
	}

	return map[string]any{
		"ID": swarmID(string(p.UID)),
		"Version": map[string]any{"Index": uint64(1)},
		"CreatedAt": p.CreationTimestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		"UpdatedAt": p.CreationTimestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		"Spec": map[string]any{
			"ContainerSpec": map[string]any{
				"Image": podImage(p),
			},
			"Placement": map[string]any{},
			"Networks":  []any{},
		},
		"ServiceID":    serviceID,
		"Slot":         slot,
		"NodeID":       nodeSwarmID,
		"Status": map[string]any{
			"State":           state,
			"Message":         statusErr,
			"Err":             statusErr,
			"Timestamp":       p.CreationTimestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"),
			"ContainerStatus": containerStatus,
			"PortStatus":      map[string]any{"Ports": []any{}},
		},
		"DesiredState":        "running",
		"NetworksAttachments": []any{},
	}
}

func podImage(p corev1.Pod) string {
	if len(p.Spec.Containers) > 0 {
		return p.Spec.Containers[0].Image
	}
	return ""
}

func kubeSecretToSwarm(s corev1.Secret) map[string]any {
	// Return the original Swarm name (which may contain underscores) so the
	// Docker CLI can match it against the name in the compose file.
	// We stored it in an annotation at create time; fall back to k8s name.
	name := s.Name
	if swarmName, ok := s.Annotations["d2k.portainer.io/swarm-name"]; ok && swarmName != "" {
		name = swarmName
	}
	return map[string]any{
		"ID": swarmID(string(s.UID)),
		"Version": map[string]any{"Index": uint64(1)},
		"CreatedAt": s.CreationTimestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		"UpdatedAt": s.CreationTimestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		"Spec": map[string]any{
			"Name":   name,
			"Labels": s.Labels,
		},
	}
}

func kubeConfigMapToSwarm(c corev1.ConfigMap) map[string]any {
	name := c.Name
	if swarmName, ok := c.Annotations["d2k.portainer.io/swarm-name"]; ok && swarmName != "" {
		name = swarmName
	}
	return map[string]any{
		"ID": swarmID(string(c.UID)),
		"Version": map[string]any{"Index": uint64(1)},
		"CreatedAt": c.CreationTimestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		"UpdatedAt": c.CreationTimestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		"Spec": map[string]any{
			"Name":   name,
			"Labels": c.Labels,
		},
	}
}

// swarmLabels merges caller-supplied labels with the d2k managed-by label.
// matchesServiceID returns true if id matches a deployment's swarm service ID,
// name, or is a prefix of either. The Docker CLI truncates IDs to 12 chars in
// tabular output; users copy that prefix and pass it to inspect/update/delete.
func matchesServiceID(d appsv1.Deployment, id string) bool {
	annotationID := d.Annotations[types.AnnotationSwarmServiceID]
	uidID := swarmID(string(d.UID))
	return annotationID == id ||
		d.Name == id ||
		uidID == id ||
		(len(id) >= 4 && strings.HasPrefix(annotationID, id)) ||
		(len(id) >= 4 && strings.HasPrefix(uidID, id))
}

func swarmLabels(extra map[string]string) map[string]string {
	labels := map[string]string{
		types.LabelManagedBy:      types.LabelManagedByValue,
		types.LabelSwarmManagedBy: types.LabelSwarmManagedByValue,
	}
	for k, v := range extra {
		labels[k] = v
	}
	return labels
}

// sanitiseResourceName lowercases and sanitises a Swarm service name for use as
// a Kubernetes resource name: lowercase, underscores to hyphens, max 63 chars.
// injectQuotaDefaults inspects the namespace ResourceQuotas and ensures that
// requests.cpu and requests.memory are set on the ResourceRequirements when the
// quota enforces them. Without this, pods are rejected 403 Forbidden by the
// quota admission controller when the caller (Docker) did not specify resources.
//
// Strategy: inject small fixed defaults (10m CPU, 32Mi memory) when the quota
// requires the field to be present but the caller didn't set it. These values
// are intentionally minimal — the quota admission controller only requires the
// field to exist, not that it reflects actual consumption. The Kubernetes
// scheduler independently handles real placement based on node capacity.
// Already-set values from the caller are never overwritten.
func (a *KubernetesDockerAdapter) injectQuotaDefaults(ctx context.Context, reqs corev1.ResourceRequirements) (corev1.ResourceRequirements, error) {
	quotas, err := a.client.CoreV1().ResourceQuotas(a.namespace).List(ctx, metav1.ListOptions{})
	if err != nil || len(quotas.Items) == 0 {
		return reqs, nil
	}

	needsCPU := false
	needsMem := false

	for _, q := range quotas.Items {
		if _, ok := q.Spec.Hard[corev1.ResourceRequestsCPU]; ok {
			needsCPU = true
		}
		if _, ok := q.Spec.Hard[corev1.ResourceRequestsMemory]; ok {
			needsMem = true
		}
	}

	if !needsCPU && !needsMem {
		return reqs, nil
	}

	if reqs.Requests == nil {
		reqs.Requests = corev1.ResourceList{}
	}

	if needsCPU {
		if _, alreadySet := reqs.Requests[corev1.ResourceCPU]; !alreadySet {
			reqs.Requests[corev1.ResourceCPU] = resource.MustParse("10m")
			a.logger.Infow("injected requests.cpu to satisfy namespace ResourceQuota",
				"namespace", a.namespace, "value", "10m")
		}
	}

	if needsMem {
		if _, alreadySet := reqs.Requests[corev1.ResourceMemory]; !alreadySet {
			reqs.Requests[corev1.ResourceMemory] = resource.MustParse("32Mi")
			a.logger.Infow("injected requests.memory to satisfy namespace ResourceQuota",
				"namespace", a.namespace, "value", "32Mi")
		}
	}

	return reqs, nil
}

func sanitiseResourceName(name string) string {
	name = strings.ToLower(strings.ReplaceAll(name, "_", "-"))
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.Trim(name, "-")
}

// intstrPtr returns a pointer to an IntOrString, needed for RollingUpdate fields.
func intstrPtr(v intstr.IntOrString) *intstr.IntOrString {
	return &v
}
