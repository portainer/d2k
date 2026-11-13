package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/portainer/d2k/internal/types"
)

// migratedConfigHash marks compose containers created by d2k 1.2.x, which
// could not keep compose's config hash: compose lists them again, and
// recreates them on the next `compose up`, as the hash never matches.
const migratedConfigHash = "d2k-migrated"

// MigrateLegacyObjects records, on the containers an earlier d2k release
// (1.2.x) created, what the current release reads: their Docker name and
// labels, and a start time. Those releases recorded neither name nor labels
// apart, so containers without a recorded Docker name are the ones to
// migrate, once. Volumes need no migration: their name falls back to the
// PVC's and their labels to legacyDockerLabels, and d2k may not patch PVCs. Objects of Swarm services are
// left as they are; in Docker host mode, finding them is logged, as the
// install probably ran Swarm mode before. A container that fails to migrate
// doesn't stop the others: the returned error joins every failure, and the
// next start retries them.
func (a *KubernetesDockerAdapter) MigrateLegacyObjects(ctx context.Context, swarmMode bool) error {
	deployments, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return fmt.Errorf("unable to list deployments: %w", err)
	}
	swarmObjects := 0
	var failures []error
	for _, d := range deployments.Items {
		if d.Labels[types.LabelSwarmManagedBy] == types.LabelSwarmManagedByValue {
			swarmObjects++
			continue
		}
		if _, recorded := d.Annotations[types.AnnotationDockerName]; recorded {
			continue
		}
		name := legacyDockerName(d.ObjectMeta)
		labels := legacyDockerLabels(d.ObjectMeta, name)
		if labels["com.docker.compose.project"] != "" && labels["com.docker.compose.config-hash"] == "" {
			labels["com.docker.compose.config-hash"] = migratedConfigHash
		}
		updates, err := recordedNameAndLabels(name, labels)
		if err != nil {
			failures = append(failures, fmt.Errorf("unable to migrate container %q: %w", name, err))
			continue
		}
		if _, ok := d.Annotations[types.AnnotationStartedAt]; !ok {
			updates[types.AnnotationStartedAt] = ptr(d.CreationTimestamp.UTC().Format(time.RFC3339Nano))
			if d.Spec.Replicas != nil && *d.Spec.Replicas == 0 {
				// Stopped under the earlier release, which recorded no
				// exit: as Docker shows a stopped container, exited (0).
				updates[types.AnnotationStoppedAt] = ptr(time.Now().UTC().Format(time.RFC3339Nano))
				updates[types.AnnotationExitCode] = ptr("0")
			}
		}
		if err := a.annotate(ctx, d.Name, updates); err != nil {
			failures = append(failures, fmt.Errorf("unable to migrate container %q: %w", name, err))
			continue
		}
		log.Info().Str("context", "Migration").Str("container", name).Msg("Recorded the Docker name and labels of a container an earlier d2k created")
	}
	if swarmObjects > 0 && !swarmMode {
		log.Warn().Str("context", "Migration").Int("deployments", swarmObjects).
			Msg("This namespace has Swarm services, which Docker host mode does not list: set D2K_SWARM_MODE=true if this install ran Swarm mode")
	}
	return errors.Join(failures...)
}

// legacyDockerName is the Docker name an earlier release kept: the workload
// name label (Kubernetes-safe: underscores became hyphens), or the object's.
func legacyDockerName(m metav1.ObjectMeta) string {
	if name := m.Labels[types.LabelWorkloadName]; name != "" {
		return name
	}
	return m.Name
}

// legacyDockerLabels are an earlier release's object labels without d2k's
// own: everything under d2k.portainer.io/, and app when d2k set it.
func legacyDockerLabels(m metav1.ObjectMeta, name string) map[string]string {
	labels := map[string]string{}
	for k, v := range m.Labels {
		if strings.HasPrefix(k, types.LabelPrefix+"/") || (k == "app" && v == name) {
			continue
		}
		labels[k] = v
	}
	return labels
}

func recordedNameAndLabels(name string, labels map[string]string) (map[string]*string, error) {
	raw, err := json.Marshal(labels)
	if err != nil {
		return nil, fmt.Errorf("unable to encode labels: %w", err)
	}
	return map[string]*string{types.AnnotationDockerName: &name, types.AnnotationDockerLabels: ptr(string(raw))}, nil
}

// MigrateSecurityTranslation brings the pods of stopped containers an
// earlier d2k created up to date with how d2k translates Docker's security
// settings now (upgradeSecurityTranslation), so Pod Security's baseline and
// restricted levels admit them as they admit a new container. A running
// container is left as it is, as changing its pod template would restart
// it; StartContainer brings it up to date when it next starts. Swarm
// services run, so they are left too: docker stack deploy rebuilds their
// pod templates. Like MigrateLegacyObjects, it runs on every start and
// returns every failure joined.
func (a *KubernetesDockerAdapter) MigrateSecurityTranslation(ctx context.Context) error {
	deployments, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return fmt.Errorf("unable to list deployments: %w", err)
	}
	var failures []error
	for _, d := range deployments.Items {
		if d.Labels[types.LabelSwarmManagedBy] == types.LabelSwarmManagedByValue || d.Spec.Replicas == nil || *d.Spec.Replicas != 0 {
			continue
		}
		if err := a.upgradeContainerSecurity(ctx, &d); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// upgradeContainerSecurity updates d's pod template when
// upgradeSecurityTranslation changes it.
func (a *KubernetesDockerAdapter) upgradeContainerSecurity(ctx context.Context, d *appsv1.Deployment) error {
	if !upgradeSecurityTranslation(&d.Spec.Template.Spec) {
		return nil
	}
	updated, err := a.client.AppsV1().Deployments(a.namespace).Update(ctx, d, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("unable to update the security settings of container %q: %w", dockerName(*d), err)
	}
	*d = *updated
	log.Info().Str("context", "Migration").Str("container", dockerName(*d)).Msg("Updated the pod security settings of a container an earlier d2k created")
	return nil
}

// upgradeSecurityTranslation changes the pod spec of a container d2k 1.3.0
// created as d2k translates the same Docker settings now, and reports
// whether it changed anything. d2k 1.3.0 added every capability the create
// request listed (Portainer's create form lists Docker's defaults, NET_RAW
// among them, which baseline refuses), set no seccomp profile, and didn't
// set runAsNonRoot for a numeric non-root user; restricted requires both.
// None of this changes what the container may do in Docker terms.
func upgradeSecurityTranslation(spec *corev1.PodSpec) bool {
	changed := false
	if spec.SecurityContext == nil {
		spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	if spec.SecurityContext.SeccompProfile == nil {
		spec.SecurityContext.SeccompProfile = runtimeDefaultSeccomp()
		changed = true
	}
	for _, c := range spec.Containers {
		sc := c.SecurityContext
		if sc == nil {
			continue
		}
		if caps := sc.Capabilities; caps != nil {
			if adds := podCapabilityAdds(caps.Add, caps.Drop); len(adds) != len(caps.Add) {
				caps.Add = adds
				changed = true
			}
		}
		if sc.RunAsUser != nil && *sc.RunAsUser != 0 && sc.RunAsNonRoot == nil {
			sc.RunAsNonRoot = new(true)
			changed = true
		}
	}
	return changed
}
