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

const nfsCsiProvisioner = "nfs.csi.k8s.io"

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

// CreateVolume implements docker volume create → PVC.
// For NFS volumes (driver_opts type=nfs), it finds a matching nfs.csi.k8s.io
// StorageClass by comparing parameters.server against the addr= in driver_opts.o.
// If no matching StorageClass is found, it returns an error that blocks the deploy.
func (a *KubernetesDockerAdapter) CreateVolume(ctx context.Context, opts CreateVolumeOptions) (*VolumeSummary, error) {
	if opts.Name == "" {
		return nil, fmt.Errorf("volume name is required")
	}

	opts.Name = sanitiseResourceName(opts.Name)
	if opts.Name == "" {
		return nil, fmt.Errorf("volume name is invalid after sanitisation")
	}

	if opts.DriverOpts["type"] == "nfs" {
		a.logger.Infow("NFS volume detected", "name", opts.Name, "opts", opts.DriverOpts)
		return a.createNFSVolume(ctx, opts)
	}

	return a.createPVCVolume(ctx, opts)
}

// createNFSVolume finds a nfs.csi.k8s.io StorageClass whose parameters.server
// matches the addr= in driver_opts.o, then creates a ReadWriteMany PVC using it.
func (a *KubernetesDockerAdapter) createNFSVolume(ctx context.Context, opts CreateVolumeOptions) (*VolumeSummary, error) {
	// Parse addr= from the o= option string (comma-separated, e.g. "addr=192.168.18.118,nolock,soft,rw").
	nfsServer := ""
	for _, part := range strings.Split(opts.DriverOpts["o"], ",") {
		if strings.HasPrefix(part, "addr=") {
			nfsServer = strings.TrimPrefix(part, "addr=")
		}
	}
	if nfsServer == "" {
		return nil, fmt.Errorf("NFS volume %q: missing addr= in driver_opts.o", opts.Name)
	}

	// Check cache first, then fall back to live StorageClass list.
	matchedSC, ok := a.nfsStorageClassForServer(nfsServer)
	if !ok {
		storageClasses, err := a.client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("NFS volume %q: unable to list StorageClasses: %w", opts.Name, err)
		}
		for _, sc := range storageClasses.Items {
			if sc.Provisioner == nfsCsiProvisioner && sc.Parameters["server"] == nfsServer {
				matchedSC = sc.Name
				a.nfsStorageClasses[nfsServer] = sc.Name
				break
			}
		}
	}

	if matchedSC == "" {
		return nil, fmt.Errorf(
			"NFS volume %q: no StorageClass with provisioner %q and server=%q found in cluster — "+
				"install nfs.csi.k8s.io and create a StorageClass with parameters.server=%s",
			opts.Name, nfsCsiProvisioner, nfsServer, nfsServer,
		)
	}

	a.logger.Infow("NFS volume matched StorageClass", "volume", opts.Name, "storageClass", matchedSC, "server", nfsServer)

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
		if len(v) <= 63 {
			labels[k] = v
		}
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: a.namespace,
			Labels:    labels,
			Annotations: map[string]string{
				"d2k.portainer.io/nfs-server": nfsServer,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			// NFS supports ReadWriteMany — multiple pods can mount simultaneously.
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: &matchedSC,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
		},
	}

	created, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			// Idempotent for stack redeploy.
			existing, getErr := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Get(ctx, opts.Name, metav1.GetOptions{})
			if getErr != nil {
				return nil, fmt.Errorf("NFS volume %q already exists", opts.Name)
			}
			return pvcToVolumeSummary(*existing), nil
		}
		return nil, fmt.Errorf("unable to create PVC for NFS volume %q: %w", opts.Name, err)
	}

	return pvcToVolumeSummary(*created), nil
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
		if len(v) <= 63 {
			labels[k] = v
		}
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
	name = sanitiseResourceName(name)

	pvc, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("volume %q not found", name)
		}
		return nil, fmt.Errorf("unable to get PVC %q: %w", name, err)
	}

	if pvc.Labels[types.LabelManagedBy] != types.LabelManagedByValue {
		return nil, fmt.Errorf("volume %q not found", name)
	}

	return pvcToVolumeSummary(*pvc), nil
}

// RemoveVolume implements docker volume rm.
func (a *KubernetesDockerAdapter) RemoveVolume(ctx context.Context, name string) error {
	name = sanitiseResourceName(name)

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

// LogNFSStorageClasses probes the cluster for nfs.csi.k8s.io StorageClasses
// at startup, logs the NFS server addresses d2k will support, and caches the
// server->storageClassName mapping for use by SwarmCreateService.
func (a *KubernetesDockerAdapter) LogNFSStorageClasses(ctx context.Context) {
	storageClasses, err := a.client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		a.logger.Warnw("unable to list StorageClasses for NFS probe", "error", err)
		return
	}

	type nfsEntry struct {
		storageClass string
		server       string
	}

	var found []nfsEntry
	for _, sc := range storageClasses.Items {
		if sc.Provisioner != nfsCsiProvisioner {
			continue
		}
		server := sc.Parameters["server"]
		if server == "" {
			continue
		}
		found = append(found, nfsEntry{storageClass: sc.Name, server: server})
		a.nfsStorageClasses[server] = sc.Name
	}

	if len(found) == 0 {
		a.logger.Infow("no nfs.csi.k8s.io StorageClasses found — NFS volumes in Compose files will be rejected")
		return
	}

	for _, e := range found {
		a.logger.Infow("NFS volume support available",
			"storageClass", e.storageClass,
			"server", e.server,
		)
	}
}

// nfsStorageClassForServer returns the StorageClass name for a given NFS server
// address, or ("", false) if none is cached.
func (a *KubernetesDockerAdapter) nfsStorageClassForServer(server string) (string, bool) {
	sc, ok := a.nfsStorageClasses[server]
	return sc, ok
}

// anyNFSStorageClass returns the first cached NFS StorageClass name, or ("", false)
// if none are available. Used when server address is unknown at service create time.
func (a *KubernetesDockerAdapter) anyNFSStorageClass() (string, bool) {
	for _, sc := range a.nfsStorageClasses {
		return sc, true
	}
	return "", false
}
