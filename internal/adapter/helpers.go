package adapter

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"

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
// sanitiseLabelValue truncates and cleans a string so it is a valid
// Kubernetes label value: max 63 chars, alphanumeric plus [-_.],
// must start and end with alphanumeric. Invalid values are dropped entirely.
func sanitiseLabelValue(v string) (string, bool) {
    if len(v) > 63 {
        v = v[:63]
    }
    // Trim leading/trailing non-alphanumeric characters.
    v = strings.TrimFunc(v, func(r rune) bool {
        return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9')
    })
    if v == "" {
        return "", false
    }
    return v, true
}
