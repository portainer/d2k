package adapter

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/portainer/d2k/internal/types"
)

// nfsVolumeConfig holds the parsed NFS connection details for a volume.
// Stored in memory so SwarmCreateService can inject inline NFS pod volumes
// without needing cluster-scoped PersistentVolume resources.
type nfsVolumeConfig struct {
	Server string
	Path   string
}

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
	Name       string
	Driver     string
	Labels     map[string]string
	SizeLimit  string
	DriverOpts map[string]string
}

// CreateVolume implements docker volume create → PVC (or in-memory NFS config).
func (a *KubernetesDockerAdapter) CreateVolume(ctx context.Context, opts CreateVolumeOptions) (*VolumeSummary, error) {
	if opts.Name == "" {
		return nil, fmt.Errorf("volume name is required")
	}

	// Sanitise: Kubernetes resource names must be RFC 1123 subdomains.
	opts.Name = sanitiseResourceName(opts.Name)
	if opts.Name == "" {
		return nil, fmt.Errorf("volume name is invalid after sanitisation")
	}

	if opts.DriverOpts["type"] == "nfs" {
		return a.createNFSVolume(ctx, opts)
	}

	return a.createPVCVolume(ctx, opts)
}

// createPVCVolume creates a standard PVC against the cluster default StorageClass.
func (a *KubernetesDockerAdapter) createPVCVolume(ctx context.Context, opts CreateVolumeOptions) (*VolumeSummary, error) {
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
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
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

// createNFSVolume stores the NFS config in memory (no cluster-scoped PV needed)
// and creates a lightweight placeholder ConfigMap so the volume survives listing.
// The actual NFS mount is injected directly into the pod spec by SwarmCreateService.
func (a *KubernetesDockerAdapter) createNFSVolume(ctx context.Context, opts CreateVolumeOptions) (*VolumeSummary, error) {
	// Parse addr= from the o= option string.
	nfsServer := ""
	for _, part := range strings.Split(opts.DriverOpts["o"], ",") {
		if strings.HasPrefix(part, "addr=") {
			nfsServer = strings.TrimPrefix(part, "addr=")
		}
	}
	if nfsServer == "" {
		return nil, fmt.Errorf("NFS volume %q: missing addr= in driver_opts.o", opts.Name)
	}

	// Strip leading colon from device path (Docker convention: ":/mnt/nfs/swarm").
	nfsPath := strings.TrimPrefix(opts.DriverOpts["device"], ":")
	if nfsPath == "" {
		return nil, fmt.Errorf("NFS volume %q: missing device in driver_opts", opts.Name)
	}

	// Store in memory so SwarmCreateService can look it up.
	a.nfsVolumesMu.Lock()
	a.nfsVolumes[opts.Name] = nfsVolumeConfig{Server: nfsServer, Path: nfsPath}
	a.nfsVolumesMu.Unlock()

	// Create a placeholder ConfigMap so the volume shows up in docker volume ls
	// and survives across service creates within the same deploy sequence.
	labels := managedLabels(opts.Name)
	for k, v := range opts.Labels {
		// Kubernetes label values must be ≤63 characters. Skip any that are longer
		// (e.g. image digest hashes passed through from docker stack deploy).
		if len(v) <= 63 {
			labels[k] = v
		}
	}
	labels["d2k.portainer.io/volume-type"] = "nfs"

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "d2k-vol-" + opts.Name,
			Namespace: a.namespace,
			Labels:    labels,
			Annotations: map[string]string{
				"d2k.portainer.io/nfs-server": nfsServer,
				"d2k.portainer.io/nfs-path":   nfsPath,
			},
		},
		Data: map[string]string{
			"server": nfsServer,
			"path":   nfsPath,
		},
	}

	if _, err := a.client.CoreV1().ConfigMaps(a.namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		if !errors.IsAlreadyExists(err) {
			// Non-fatal — the in-memory entry is what matters for pod injection.
			a.logger.Warnw("unable to create NFS volume placeholder ConfigMap", "volume", opts.Name, "err", err)
		} else {
			// On redeploy, reload the config from the existing ConfigMap into memory.
			existing, getErr := a.client.CoreV1().ConfigMaps(a.namespace).Get(ctx, "d2k-vol-"+opts.Name, metav1.GetOptions{})
			if getErr == nil {
				a.nfsVolumesMu.Lock()
				a.nfsVolumes[opts.Name] = nfsVolumeConfig{
					Server: existing.Data["server"],
					Path:   existing.Data["path"],
				}
				a.nfsVolumesMu.Unlock()
			}
		}
	}

	return &VolumeSummary{
		Name:       opts.Name,
		Driver:     "nfs",
		Mountpoint: nfsServer + ":" + nfsPath,
		Labels:     labels,
	}, nil
}

// nfsConfigForVolume returns the NFS config for a volume name if one exists.
// Checks in-memory first, then falls back to the placeholder ConfigMap.
func (a *KubernetesDockerAdapter) nfsConfigForVolume(ctx context.Context, name string) (nfsVolumeConfig, bool) {
	a.nfsVolumesMu.RLock()
	cfg, ok := a.nfsVolumes[name]
	a.nfsVolumesMu.RUnlock()
	if ok {
		return cfg, true
	}

	// Fallback: check the placeholder ConfigMap (e.g. after a d2k restart).
	cm, err := a.client.CoreV1().ConfigMaps(a.namespace).Get(ctx, "d2k-vol-"+name, metav1.GetOptions{})
	if err != nil {
		return nfsVolumeConfig{}, false
	}
	server := cm.Data["server"]
	path := cm.Data["path"]
	if server == "" || path == "" {
		return nfsVolumeConfig{}, false
	}

	cfg = nfsVolumeConfig{Server: server, Path: path}
	a.nfsVolumesMu.Lock()
	a.nfsVolumes[name] = cfg
	a.nfsVolumesMu.Unlock()

	return cfg, true
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

	// Include NFS volumes (tracked via placeholder ConfigMaps).
	cms, err := a.client.CoreV1().ConfigMaps(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "d2k.portainer.io/volume-type=nfs," + types.LabelManagedBy + "=" + types.LabelManagedByValue,
	})
	if err == nil {
		for _, cm := range cms.Items {
			volName := strings.TrimPrefix(cm.Name, "d2k-vol-")
			summaries = append(summaries, VolumeSummary{
				Name:       volName,
				Driver:     "nfs",
				Mountpoint: cm.Data["server"] + ":" + cm.Data["path"],
				Labels:     cm.Labels,
				CreatedAt:  cm.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
			})
		}
	}

	return summaries, nil
}

// InspectVolume implements docker volume inspect.
func (a *KubernetesDockerAdapter) InspectVolume(ctx context.Context, name string) (*VolumeSummary, error) {
	name = sanitiseResourceName(name)

	pvc, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		if pvc.Labels[types.LabelManagedBy] != types.LabelManagedByValue {
			return nil, fmt.Errorf("volume %q not found", name)
		}
		return pvcToVolumeSummary(*pvc), nil
	}

	// Check NFS placeholder ConfigMap.
	if cfg, ok := a.nfsConfigForVolume(ctx, name); ok {
		return &VolumeSummary{
			Name:       name,
			Driver:     "nfs",
			Mountpoint: cfg.Server + ":" + cfg.Path,
		}, nil
	}

	return nil, fmt.Errorf("volume %q not found", name)
}

// RemoveVolume implements docker volume rm.
func (a *KubernetesDockerAdapter) RemoveVolume(ctx context.Context, name string) error {
	name = sanitiseResourceName(name)

	// Try PVC first.
	pvc, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil && pvc.Labels[types.LabelManagedBy] == types.LabelManagedByValue {
		return a.client.CoreV1().PersistentVolumeClaims(a.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	}

	// Try NFS placeholder ConfigMap.
	cmName := "d2k-vol-" + name
	if err := a.client.CoreV1().ConfigMaps(a.namespace).Delete(ctx, cmName, metav1.DeleteOptions{}); err == nil {
		a.nfsVolumesMu.Lock()
		delete(a.nfsVolumes, name)
		a.nfsVolumesMu.Unlock()
		return nil
	}

	return fmt.Errorf("volume %q not found", name)
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
