package adapter

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/portainer/d2k/internal/types"
)

// VolumeSummary is a Docker-compatible volume entry for docker volume ls.
type VolumeSummary struct {
	Name       string
	Driver     string
	Mountpoint string
	Labels     map[string]string
	CreatedAt  string
}

// CreateVolumeOptions mirrors docker volume create flags.
type CreateVolumeOptions struct {
	// Name is the volume name. Required.
	Name string
	// Driver is ignored — we always use the cluster default StorageClass.
	Driver string
	// Labels are user-supplied labels.
	Labels map[string]string
	// SizeLimit allows an optional storage request size, e.g. "1Gi".
	// If empty, defaults to "1Gi".
	SizeLimit string
}

// CreateVolume implements docker volume create → PVC.
func (a *KubernetesDockerAdapter) CreateVolume(ctx context.Context, opts CreateVolumeOptions) (*VolumeSummary, error) {
	if opts.Name == "" {
		return nil, fmt.Errorf("volume name is required")
	}

	size := opts.SizeLimit
	if size == "" {
		size = "1Gi"
	}

	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return nil, fmt.Errorf("invalid size %q: %w", size, err)
	}

	labels := managedLabels(opts.Name)
	for k, v := range opts.Labels {
		labels[k] = v
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: a.namespace,
			Labels:    labels,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			// ReadWriteOnce matches Docker volume semantics — one writer at a time.
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: qty,
				},
			},
			// Intentionally no StorageClassName set — uses cluster default.
		},
	}

	created, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("volume %q already exists", opts.Name)
		}
		return nil, fmt.Errorf("unable to create PVC %q: %w", opts.Name, err)
	}

	return pvcToVolumeSummary(*created), nil
}

// ListVolumes implements docker volume ls.
func (a *KubernetesDockerAdapter) ListVolumes(ctx context.Context) ([]VolumeSummary, error) {
	pvcs, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return nil, fmt.Errorf("unable to list PVCs: %w", err)
	}

	summaries := make([]VolumeSummary, 0, len(pvcs.Items))
	for _, pvc := range pvcs.Items {
		summaries = append(summaries, *pvcToVolumeSummary(pvc))
	}

	return summaries, nil
}

// InspectVolume implements docker volume inspect.
func (a *KubernetesDockerAdapter) InspectVolume(ctx context.Context, name string) (*VolumeSummary, error) {
	pvc, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("volume %q not found", name)
		}
		return nil, fmt.Errorf("unable to get PVC %q: %w", name, err)
	}

	// Only return volumes managed by d2k.
	if pvc.Labels[types.LabelManagedBy] != types.LabelManagedByValue {
		return nil, fmt.Errorf("volume %q not found", name)
	}

	return pvcToVolumeSummary(*pvc), nil
}

// RemoveVolume implements docker volume rm.
func (a *KubernetesDockerAdapter) RemoveVolume(ctx context.Context, name string) error {
	// Confirm it's d2k-managed before deleting.
	if _, err := a.InspectVolume(ctx, name); err != nil {
		return err
	}

	if err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("unable to delete PVC %q: %w", name, err)
	}

	return nil
}

// pvcToVolumeSummary converts a PVC to the Docker VolumeSummary shape.
func pvcToVolumeSummary(pvc corev1.PersistentVolumeClaim) *VolumeSummary {
	return &VolumeSummary{
		Name:       pvc.Name,
		Driver:     "d2k",
		Mountpoint: "/var/lib/d2k/volumes/" + pvc.Name,
		Labels:     pvc.Labels,
		CreatedAt:  pvc.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
	}
}
