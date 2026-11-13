package adapter

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Pod Security levels, as the namespace enforces them (PodSecurityLevel).
const (
	PodSecurityPrivileged = "privileged"
	PodSecurityBaseline   = "baseline"
	PodSecurityRestricted = "restricted"
	// PodSecurityUnknown: the level couldn't be worked out.
	PodSecurityUnknown = "unknown"
)

// podSecurityLevelTTL is how long PodSecurityLevel keeps a level, so a
// change of the namespace's labels shows within a minute, without
// watching the namespace.
const podSecurityLevelTTL = time.Minute

// podSecurityProbeTimeout bounds the dry run, which /info waits for.
const podSecurityProbeTimeout = 5 * time.Second

// podSecurityProbe is the pod whose dry run tells the namespace's level: a
// privileged pod is refused by baseline and restricted, and Pod Security's
// refusal names the level.
var podSecurityProbe = corev1.PodSpec{Containers: []corev1.Container{{
	Name:            "probe",
	Image:           "d2k-pod-security-probe",
	SecurityContext: &corev1.SecurityContext{Privileged: new(true)},
}}}

// podSecurityLevel caches the level PodSecurityLevel worked out.
type podSecurityLevel struct {
	level     string
	checkedAt time.Time
}

// PodSecurityLevel returns the Pod Security level the namespace enforces,
// whether set by its labels or by the cluster's default (Talos sets
// baseline), which the API doesn't show. It submits a pod as a dry run, and
// never changes the level.
func (a *KubernetesDockerAdapter) PodSecurityLevel(ctx context.Context) string {
	a.podSecurityMu.Lock()
	defer a.podSecurityMu.Unlock()
	if !a.podSecurity.checkedAt.IsZero() && time.Since(a.podSecurity.checkedAt) < podSecurityLevelTTL {
		return a.podSecurity.level
	}
	// A client that goes away mustn't leave "unknown" cached.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), podSecurityProbeTimeout)
	defer cancel()
	level := a.probePodSecurityLevel(ctx)
	a.podSecurity = podSecurityLevel{level: level, checkedAt: time.Now()}
	return level
}

func (a *KubernetesDockerAdapter) probePodSecurityLevel(ctx context.Context) string {
	if a.dryRunPod == nil {
		return PodSecurityUnknown
	}
	_, err := a.dryRunPod(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "d2k-pod-security-probe-", Namespace: a.namespace},
		Spec:       *podSecurityProbe.DeepCopy(),
	})
	if err == nil {
		return PodSecurityPrivileged
	}
	if m := podSecurityMessage.FindStringSubmatch(err.Error()); m != nil && apierrors.IsForbidden(err) {
		return m[1]
	}
	// Another policy refused it, or the dry run failed: Pod Security's
	// own verdict is hidden.
	log.Debug().Str("context", "KubernetesAdapter").Err(err).Msg("Unable to work out the namespace's Pod Security level")
	return PodSecurityUnknown
}

// PodSecurityWarning says what the namespace's Pod Security level refuses,
// and the Docker settings that comply. Empty when it refuses nothing d2k
// can tell.
func PodSecurityWarning(namespace, level string) string {
	switch level {
	case PodSecurityBaseline:
		return fmt.Sprintf("namespace %s enforces Pod Security %q: it refuses --privileged, host paths, host ports, unconfined seccomp or AppArmor, and --cap-add of capabilities Docker doesn't grant by default", namespace, level)
	case PodSecurityRestricted:
		return fmt.Sprintf("namespace %s enforces Pod Security %q: containers need a numeric non-root user (--user 1000:1000), --cap-drop ALL and --security-opt no-new-privileges, and images that run as that user", namespace, level)
	}
	return ""
}

// LogPodSecurityLevel logs the level the namespace enforces, and what
// containers need under it.
func (a *KubernetesDockerAdapter) LogPodSecurityLevel(ctx context.Context) {
	level := a.PodSecurityLevel(ctx)
	if warning := PodSecurityWarning(a.namespace, level); warning != "" {
		log.Warn().Str("context", "Startup").Str("level", level).Msg(warning)
		return
	}
	if level == PodSecurityUnknown {
		log.Warn().Str("context", "Startup").Msgf("Unable to work out the Pod Security level namespace %s enforces", a.namespace)
		return
	}
	log.Info().Str("context", "Startup").Str("level", level).Msgf("Namespace %s enforces Pod Security %q", a.namespace, level)
}
