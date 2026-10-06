package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"

	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/internal/types"
)

// Docker networks are recorded as annotations on d2k's namespace, one per
// network. d2k's Role lets it patch its namespace; an admission policy
// (deploy/kubernetes.yaml) lets it change only these annotations there.

// networkAnnotation is the namespace annotation that records the network.
// Annotation names are at most 63 characters, so it is named by a digest.
func networkAnnotation(name string) string {
	sum := sha256.Sum256([]byte(name))
	return types.AnnotationNetworkPrefix + hex.EncodeToString(sum[:8])
}

// networkRecords returns the networks recorded on the namespace.
func (a *KubernetesDockerAdapter) networkRecords(ctx context.Context) ([]networkRecord, error) {
	ns, err := a.client.CoreV1().Namespaces().Get(ctx, a.namespace, metav1GetOptions())
	if err != nil {
		return nil, fmt.Errorf("unable to read the networks of namespace %q: %w", a.namespace, err)
	}
	var records []networkRecord
	for key, value := range ns.Annotations {
		var r networkRecord
		if strings.HasPrefix(key, types.AnnotationNetworkPrefix) && json.Unmarshal([]byte(value), &r) == nil {
			records = append(records, r)
		}
	}
	return records, nil
}

// recordNetwork records a new network, or fails with a conflict if a network
// of that name exists, as Docker does. The patch is conditional on the
// namespace's resource version, so concurrent changes are never overwritten.
func (a *KubernetesDockerAdapter) recordNetwork(ctx context.Context, r networkRecord) error {
	value, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return a.patchNamespace(ctx, func(annotations map[string]string) ([]patchOp, error) {
		key := networkAnnotation(r.Name)
		if _, exists := annotations[key]; exists {
			return nil, errdefs.Conflict(fmt.Errorf("network with name %s already exists", r.Name))
		}
		if annotations == nil {
			return []patchOp{{Op: "add", Path: "/metadata/annotations", Value: map[string]string{key: string(value)}}}, nil
		}
		return []patchOp{{Op: "add", Path: "/metadata/annotations/" + jsonPointerEscape(key), Value: string(value)}}, nil
	})
}

// removeNetworks deletes the records of the networks that match.
func (a *KubernetesDockerAdapter) removeNetworks(ctx context.Context, match func(networkRecord) bool) error {
	return a.patchNamespace(ctx, func(annotations map[string]string) ([]patchOp, error) {
		var ops []patchOp
		for key, value := range annotations {
			var r networkRecord
			if strings.HasPrefix(key, types.AnnotationNetworkPrefix) && json.Unmarshal([]byte(value), &r) == nil && match(r) {
				ops = append(ops, patchOp{Op: "remove", Path: "/metadata/annotations/" + jsonPointerEscape(key)})
			}
		}
		return ops, nil
	})
}

type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"` // ignored by remove
}

// namespaceBackoff spaces the retries of concurrent namespace patches, such
// as those of compose projects that create networks at the same time.
var namespaceBackoff = wait.Backoff{Steps: 12, Duration: 20 * time.Millisecond, Factor: 1.5, Jitter: 1, Cap: 2 * time.Second}

// patchNamespace applies the operations ops returns for the namespace's
// current annotations, retrying when the namespace changed meanwhile.
func (a *KubernetesDockerAdapter) patchNamespace(ctx context.Context, ops func(map[string]string) ([]patchOp, error)) error {
	err := retry.OnError(namespaceBackoff, func(err error) bool {
		return apierrors.IsConflict(err) || apierrors.IsInvalid(err)
	}, func() error {
		ns, err := a.client.CoreV1().Namespaces().Get(ctx, a.namespace, metav1GetOptions())
		if err != nil {
			return fmt.Errorf("unable to read namespace %q: %w", a.namespace, err)
		}
		changes, err := ops(ns.Annotations)
		if err != nil || len(changes) == 0 {
			return err
		}
		patch := append([]patchOp{{Op: "test", Path: "/metadata/resourceVersion", Value: ns.ResourceVersion}}, changes...)
		body, err := json.Marshal(patch)
		if err != nil {
			return err
		}
		_, err = a.client.CoreV1().Namespaces().Patch(ctx, a.namespace, k8stypes.JSONPatchType, body, metav1.PatchOptions{})
		// A failed resourceVersion test is a 422 (Invalid) with no details,
		// as is a patch the API server rejects, such as one that takes the
		// annotations past their size limit: only the first is retried.
		if apierrors.IsInvalid(err) {
			if now, getErr := a.client.CoreV1().Namespaces().Get(ctx, a.namespace, metav1GetOptions()); getErr == nil && now.ResourceVersion == ns.ResourceVersion {
				return errdefs.InvalidParameter(fmt.Errorf("unable to record the network on namespace %q: %s", a.namespace, err.Error()))
			}
		}
		return err
	})
	if apierrors.IsConflict(err) || apierrors.IsInvalid(err) {
		return errdefs.Unavailable(fmt.Errorf("namespace %q kept changing while recording the network: try again", a.namespace))
	}
	return err
}

// jsonPointerEscape escapes a key for a JSON Pointer (RFC 6901).
func jsonPointerEscape(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}
