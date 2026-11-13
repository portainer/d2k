package adapter

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/rs/zerolog/log"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/portainer/d2k/internal/errdefs"
)

// podSecurityMessage matches Pod Security's refusal ("violates PodSecurity")
// and warning ("would violate PodSecurity"), capturing the level and the
// checks that failed.
var podSecurityMessage = regexp.MustCompile(`(?:violates|would violate) PodSecurity "([^":]+)(?::[^"]*)?": (.*)$`)

// clientWarningCode is the code of the Warning headers meant for the
// client (RFC 7234); client-go passes on only these.
const clientWarningCode = 299

// admissionWarningsKey is the context key of the slice a dry run's warnings
// are collected into (admissionWarnings).
type admissionWarningsKey struct{}

// admissionWarnings collects the warnings of a request into the slice in
// its context, so each dry run gets its own.
type admissionWarnings struct{}

func (admissionWarnings) HandleWarningHeaderWithContext(ctx context.Context, code int, _, text string) {
	if code != clientWarningCode {
		return
	}
	if warnings, ok := ctx.Value(admissionWarningsKey{}).(*[]string); ok {
		*warnings = append(*warnings, text)
	}
}

// newPodDryRun returns a function that submits a pod as a server-side dry
// run, as admission sees it, and returns the warnings admission gave.
func newPodDryRun(cfg *rest.Config) (func(context.Context, *corev1.Pod) ([]string, error), error) {
	cfg = rest.CopyConfig(cfg)
	cfg.WarningHandlerWithContext = admissionWarnings{}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, pod *corev1.Pod) ([]string, error) {
		var warnings []string
		_, err := client.CoreV1().Pods(pod.Namespace).Create(context.WithValue(ctx, admissionWarningsKey{}, &warnings), pod,
			metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		return warnings, err
	}, nil
}

// checkAdmission submits a pod of tmpl as a dry run, so a container the
// namespace's policies refuse is refused before anything is created, and
// returns the warnings they give. Pod Security's refusals and warnings name
// the Docker settings to change.
func (a *KubernetesDockerAdapter) checkAdmission(ctx context.Context, name string, tmpl corev1.PodTemplateSpec) ([]string, error) {
	if a.dryRunPod == nil {
		return nil, nil
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: name + "-", Namespace: a.namespace, Labels: tmpl.Labels, Annotations: tmpl.Annotations},
		Spec:       tmpl.Spec,
	}
	return admissionResult(a.dryRunPod(ctx, pod))
}

// admissionResult translates a dry run's result. Only a policy's refusal
// refuses the container: anything else, such as a full quota or a webhook
// that takes no dry runs, is left to the real create or start.
func admissionResult(warnings []string, err error) ([]string, error) {
	for i, w := range warnings {
		if m := podSecurityMessage.FindStringSubmatch(w); m != nil && strings.HasPrefix(w, "would") {
			warnings[i] = fmt.Sprintf("the namespace's Pod Security level %q would refuse this container, and only warns: %s", m[1], withAdvice(m[2], w))
		}
	}
	if err == nil {
		return warnings, nil
	}
	if apierrors.IsForbidden(err) {
		msg := err.Error()
		if m := podSecurityMessage.FindStringSubmatch(msg); m != nil {
			reason := msg[strings.Index(msg, "violates PodSecurity"):]
			return nil, errdefs.Forbidden(fmt.Errorf("the namespace's Pod Security level %q refuses this container: %s", m[1], withAdvice(m[2], reason)))
		}
		if strings.Contains(msg, "admission webhook") || strings.Contains(msg, "ValidatingAdmissionPolicy") {
			return nil, errdefs.Forbidden(err)
		}
	}
	log.Debug().Str("context", "KubernetesAdapter").Err(err).Msg("Admission dry run failed, not on a policy; the create or start reports any refusal")
	return warnings, nil
}

// withAdvice returns the Docker settings that fix Pod Security's failed
// checks, then Kubernetes' own words.
func withAdvice(checks, kubernetes string) string {
	var advice []string
	for _, check := range splitChecks(checks) {
		name, detail, _ := strings.Cut(check, " (")
		if a := podSecurityAdvice(name, strings.TrimSuffix(detail, ")")); a != "" && !slices.Contains(advice, a) {
			advice = append(advice, a)
		}
	}
	if len(advice) == 0 {
		return "Kubernetes: " + kubernetes
	}
	return strings.Join(advice, "; ") + " (Kubernetes: " + kubernetes + ")"
}

// splitChecks splits Pod Security's list of failed checks, "name (detail),
// name (detail)", whose details may hold commas.
func splitChecks(checks string) []string {
	var parts []string
	depth, start := 0, 0
	for i, r := range checks {
		switch {
		case r == '(':
			depth++
		case r == ')':
			depth--
		case r == ',' && depth == 0:
			parts = append(parts, strings.TrimSpace(checks[start:i]))
			start = i + 1
		}
	}
	return append(parts, strings.TrimSpace(checks[start:]))
}

// podSecurityAdvice names the Docker flag and compose key that fix a failed
// Pod Security check, for the settings d2k translates into the pod. Other
// checks get Kubernetes' words alone.
func podSecurityAdvice(check, detail string) string {
	switch check {
	case "privileged":
		return "remove --privileged (compose: privileged)"
	case "non-default capabilities":
		return "remove " + addedCapabilityNames(detail) + " from --cap-add (compose: cap_add)"
	case "unrestricted capabilities":
		if strings.Contains(detail, "capabilities.drop") {
			return "add --cap-drop ALL (compose: cap_drop: [ALL])"
		}
		return "remove " + addedCapabilityNames(detail) + " from --cap-add (compose: cap_add); only NET_BIND_SERVICE may be added"
	case "allowPrivilegeEscalation != false":
		return "add --security-opt no-new-privileges (compose: security_opt: [no-new-privileges:true])"
	case "runAsNonRoot != true", "runAsUser=0":
		return `run as a numeric non-root user, such as --user 1000:1000 (compose: user: "1000:1000")`
	case "seccompProfile":
		if strings.Contains(detail, "must not set") {
			return "remove --security-opt seccomp=unconfined (compose: security_opt: [seccomp:unconfined])"
		}
		// d2k gives every pod Docker's default profile: only a container
		// created by an earlier d2k lacks it.
		return "re-create the container, so it gets Docker's default seccomp profile"
	case "forbidden AppArmor profile":
		return "remove --security-opt apparmor=unconfined (compose: security_opt: [apparmor:unconfined])"
	case "seLinuxOptions":
		return "remove --security-opt label=... (compose: security_opt: [label:...])"
	case "forbidden sysctls":
		return "remove --sysctl " + detail + " (compose: sysctls)"
	case "hostPath volumes":
		return "use a named volume instead of a host path (compose: volumes)"
	case "hostPort":
		return "publish the port in ingress mode, not host mode (compose: ports: mode: host)"
	}
	return ""
}

// addedCapabilityNames returns the capabilities a check's detail names,
// `container "app" must not include "NET_RAW", "SYS_ADMIN" in ...`, as
// "NET_RAW, SYS_ADMIN".
func addedCapabilityNames(detail string) string {
	_, names, _ := strings.Cut(detail, "must not include ")
	names, _, _ = strings.Cut(names, " in ")
	return strings.ReplaceAll(names, `"`, "")
}
