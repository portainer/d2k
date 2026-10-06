package adapter

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/portainer/d2k/internal/types"
)

// metav1GetOptions returns a default metav1.GetOptions.
func metav1GetOptions() metav1.GetOptions {
	return metav1.GetOptions{}
}

// metav1ListOptions returns a ListOptions that selects only d2k-managed resources.
func metav1ListOptions() metav1.ListOptions {
	return metav1.ListOptions{
		LabelSelector: types.LabelManagedBy + "=" + types.LabelManagedByValue,
	}
}

// podsOf selects the pods d2k created for the named Deployment: other
// workloads in the namespace may carry the same app label.
func podsOf(name string) metav1.ListOptions {
	opts := metav1ListOptions()
	opts.LabelSelector += ",app=" + name
	return opts
}

// managedLabels returns the baseline labels applied to every Kubernetes resource
// that d2k creates.
func managedLabels(name string) map[string]string {
	return map[string]string{
		types.LabelManagedBy:    types.LabelManagedByValue,
		types.LabelWorkloadName: name,
		// app label for selector convenience
		"app": name,
	}
}

// addDockerLabels adds the Docker labels that are also valid Kubernetes
// labels to labels. The others stay Docker labels only: they are recorded
// verbatim in an annotation.
func addDockerLabels(labels, docker map[string]string) {
	for k, v := range docker {
		if clean, ok := sanitiseLabelValue(v); ok && len(validation.IsQualifiedName(k)) == 0 {
			labels[k] = clean
		}
	}
}

// Kubernetes label values must be 63 chars max, match [A-Za-z0-9._-]*,
// and start/end with an alphanumeric character.
func sanitiseLabelValue(v string) (string, bool) {
	if len(v) > 63 {
		return "", false
	}
	if v == "" {
		return v, true
	}
	isAlnum := func(r rune) bool {
		return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9'
	}
	for _, r := range v {
		if !isAlnum(r) && r != '-' && r != '_' && r != '.' {
			return "", false
		}
	}
	// Must start and end with alphanumeric.
	if !isAlnum(rune(v[0])) || !isAlnum(rune(v[len(v)-1])) {
		return "", false
	}
	return v, true
}
