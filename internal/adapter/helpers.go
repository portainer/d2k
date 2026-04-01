package adapter

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

// Kubernetes label values must be 63 chars max, match [A-Za-z0-9._-]*,
// and start/end with an alphanumeric character.
func sanitiseLabelValue(v string) (string, bool) {
    if len(v) > 63 {
        return "", false
    }
    if v == "" {
        return v, true
    }
    for _, r := range v {
        if !('a' <= r && r <= 'z') && !('A' <= r && r <= 'Z') && !('0' <= r && r <= '9') && r != '-' && r != '_' && r != '.' {
            return "", false
        }
    }
    // Must start and end with alphanumeric.
    first := rune(v[0])
    last := rune(v[len(v)-1])
    if !('a' <= first && first <= 'z') && !('A' <= first && first <= 'Z') && !('0' <= first && first <= '9') {
        return "", false
    }
    if !('a' <= last && last <= 'z') && !('A' <= last && last <= 'Z') && !('0' <= last && last <= '9') {
        return "", false
    }
    return v, true
}
