package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/moby/moby/api/types/volume"
	"github.com/rs/zerolog/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/internal/types"
)

const nfsCsiProvisioner = "nfs.csi.k8s.io"

// volumeTerminationTimeout bounds how long createPVC waits for an old PVC of
// the same name to finish terminating before giving up. Kubernetes'
// pvc-protection finalizer holds a PVC until the pod that mounted it is
// gone, which can take as long as that pod's termination grace period —
// corev1.DefaultTerminationGracePeriodSeconds, 30s — if the container does
// not exit as soon as it is sent SIGTERM (docker rm -f deletes the
// Deployment but does not force-kill the pod). Measured live: a container
// that ignored SIGTERM kept its PVC terminating for about 33s end to end.
var volumeTerminationTimeout = 30 * time.Second

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
func (a *KubernetesDockerAdapter) CreateVolume(ctx context.Context, opts CreateVolumeOptions) (*volume.Volume, error) {
	if opts.Name == "" {
		return nil, errdefs.InvalidParameter(fmt.Errorf("volume name is required"))
	}

	if opts.Driver != "" && opts.Driver != "local" {
		return nil, errdefs.InvalidParameter(fmt.Errorf("volume driver %q is not supported by d2k: volumes are PersistentVolumeClaims of the default StorageClass", opts.Driver))
	}
	if sanitiseResourceName(opts.Name) == "" {
		return nil, errdefs.InvalidParameter(fmt.Errorf("invalid volume name %q", opts.Name))
	}

	if opts.DriverOpts["type"] == "nfs" {
		log.Info().Str("context", "KubernetesAdapter").Any("name", opts.Name).Any("opts", opts.DriverOpts).Msg("NFS volume detected")
		return a.createNFSVolume(ctx, opts)
	}

	return a.createPVCVolume(ctx, opts)
}

// createNFSVolume finds a nfs.csi.k8s.io StorageClass whose parameters.server
// matches the addr= in driver_opts.o, then creates a ReadWriteMany PVC using it.
func (a *KubernetesDockerAdapter) createNFSVolume(ctx context.Context, opts CreateVolumeOptions) (*volume.Volume, error) {
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
				a.cacheNFSStorageClass(nfsServer, sc.Name)
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

	log.Info().Str("context", "KubernetesAdapter").Any("volume", opts.Name).Any("storage_class", matchedSC).Any("server", nfsServer).Msg("NFS volume matched StorageClass")

	size := opts.SizeLimit
	if size == "" {
		size = "1Gi"
	}
	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return nil, errdefs.InvalidParameter(fmt.Errorf("invalid size %q: %w", size, err))
	}

	meta, err := a.volumeMeta(opts)
	if err != nil {
		return nil, err
	}
	meta.Annotations["d2k.portainer.io/nfs-server"] = nfsServer

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: meta,
		Spec: corev1.PersistentVolumeClaimSpec{
			// NFS supports ReadWriteMany — multiple pods can mount simultaneously.
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: &matchedSC,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
		},
	}

	return a.createPVC(ctx, opts.Name, pvc)
}

// createPVCVolume creates a standard PVC against the cluster default StorageClass.
func (a *KubernetesDockerAdapter) createPVCVolume(ctx context.Context, opts CreateVolumeOptions) (*volume.Volume, error) {
	size := opts.SizeLimit
	if size == "" {
		size = "1Gi"
	}

	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return nil, errdefs.InvalidParameter(fmt.Errorf("invalid size %q: %w", size, err))
	}

	meta, err := a.volumeMeta(opts)
	if err != nil {
		return nil, err
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: meta,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
		},
	}

	return a.createPVC(ctx, opts.Name, pvc)
}

// volumeMeta returns the metadata of the PVC for a new volume. The Docker
// name, labels and driver options are recorded verbatim in annotations, as
// Kubernetes names and label values cannot hold all of them.
func (a *KubernetesDockerAdapter) volumeMeta(opts CreateVolumeOptions) (metav1.ObjectMeta, error) {
	name := sanitiseResourceName(opts.Name)
	labels := managedLabels(name)
	addDockerLabels(labels, opts.Labels)
	annotations := map[string]string{types.AnnotationDockerName: opts.Name}
	for key, value := range map[string]map[string]string{types.AnnotationDockerLabels: opts.Labels, types.AnnotationDriverOpts: opts.DriverOpts} {
		if len(value) == 0 {
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return metav1.ObjectMeta{}, err
		}
		annotations[key] = string(raw)
	}
	return metav1.ObjectMeta{Name: name, Namespace: a.namespace, Labels: labels, Annotations: annotations}, nil
}

// createPVC creates the PVC for volume name. Creating a volume that exists
// returns it, as in Docker. If a PVC of the same name is still terminating,
// it waits up to volumeTerminationTimeout for the delete to finish — PVC
// deletion is asynchronous, unlike Docker's synchronous local driver — and
// retries the create, rather than handing back a claim about to be garbage
// collected or refusing a recreate Docker would have let through.
func (a *KubernetesDockerAdapter) createPVC(ctx context.Context, name string, pvc *corev1.PersistentVolumeClaim) (*volume.Volume, error) {
	created, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		old, getErr := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Get(ctx, pvc.Name, metav1.GetOptions{})
		if getErr == nil && old.DeletionTimestamp != nil {
			if !a.waitForPVCGone(ctx, old, volumeTerminationTimeout) {
				return nil, errdefs.Conflict(fmt.Errorf("volume %q is being deleted: try again once it is gone", name))
			}
			created, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Create(ctx, pvc, metav1.CreateOptions{})
			if err == nil {
				return pvcToVolume(*created), nil
			}
			// Another create of the volume, waiting too, made it first.
			if !errors.IsAlreadyExists(err) {
				return nil, fmt.Errorf("unable to create PVC %q: %w", pvc.Name, err)
			}
		}
		existing, err := a.InspectVolume(ctx, name)
		if errdefs.IsNotFound(err) {
			if old, getErr := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Get(ctx, pvc.Name, metav1.GetOptions{}); getErr == nil {
				return nil, volumeNameConflict(name, *old)
			}
			return nil, errdefs.Conflict(fmt.Errorf("volume name %q is used by a PersistentVolumeClaim not managed by d2k", name))
		}
		return existing, err
	}
	if err != nil {
		return nil, fmt.Errorf("unable to create PVC %q: %w", pvc.Name, err)
	}
	return pvcToVolume(*created), nil
}

// volumeNameConflict is the error for volume name, whose PVC name is
// already pvc's: a PVC not managed by d2k, or another volume's. Volume
// names that differ only by case, _ or - map to the same PVC name.
func volumeNameConflict(name string, pvc corev1.PersistentVolumeClaim) error {
	if pvc.Labels[types.LabelManagedBy] != types.LabelManagedByValue {
		return errdefs.Conflict(fmt.Errorf("volume name %q is used by a PersistentVolumeClaim not managed by d2k", name))
	}
	return errdefs.Conflict(fmt.Errorf("volume name %q conflicts with volume %q: d2k stores both as PersistentVolumeClaim %q", name, recordedName(pvc.ObjectMeta), pvc.Name))
}

// waitForPVCGone waits until the PVC old no longer exists, or timeout
// passes. It returns whether the PVC is confirmed gone: deleted, or replaced
// by a new PVC of the same name.
func (a *KubernetesDockerAdapter) waitForPVCGone(ctx context.Context, old *corev1.PersistentVolumeClaim, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		pvc, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Get(ctx, old.Name, metav1.GetOptions{})
		if errors.IsNotFound(err) || err == nil && pvc.UID != old.UID {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// ListVolumes implements docker volume ls.
func (a *KubernetesDockerAdapter) ListVolumes(ctx context.Context) ([]volume.Volume, error) {
	pvcs, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return nil, fmt.Errorf("unable to list PVCs: %w", err)
	}

	vols := make([]volume.Volume, 0, len(pvcs.Items))
	for _, pvc := range pvcs.Items {
		vols = append(vols, *pvcToVolume(pvc))
	}

	return vols, nil
}

// ReferencedVolumes returns the names of the volumes a container, running or
// stopped, mounts: Docker's referenced volumes, the ones that are not dangling.
func (a *KubernetesDockerAdapter) ReferencedVolumes(ctx context.Context) (map[string]bool, error) {
	deployments, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return nil, fmt.Errorf("unable to list deployments: %w", err)
	}
	claims := map[string]bool{}
	for _, d := range deployments.Items {
		for _, v := range d.Spec.Template.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				claims[v.PersistentVolumeClaim.ClaimName] = true
			}
		}
	}
	pvcs, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return nil, fmt.Errorf("unable to list PVCs: %w", err)
	}
	referenced := map[string]bool{}
	for _, pvc := range pvcs.Items {
		if claims[pvc.Name] {
			referenced[recordedName(pvc.ObjectMeta)] = true
		}
	}
	return referenced, nil
}

// InspectVolume implements docker volume inspect.
func (a *KubernetesDockerAdapter) InspectVolume(ctx context.Context, name string) (*volume.Volume, error) {
	notFound := errdefs.NotFound(fmt.Errorf("no such volume: %s", name))
	pvc, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Get(ctx, sanitiseResourceName(name), metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil, notFound
	}
	if err != nil {
		return nil, fmt.Errorf("unable to get PVC for volume %q: %w", name, err)
	}

	// Volumes whose Docker names map to the same PVC name are different
	// volumes; volumes created before the name was recorded match any of them.
	_, recorded := pvc.Annotations[types.AnnotationDockerName]
	if pvc.Labels[types.LabelManagedBy] != types.LabelManagedByValue || (recorded && recordedName(pvc.ObjectMeta) != name) {
		return nil, notFound
	}

	return pvcToVolume(*pvc), nil
}

// RemoveVolume implements docker volume rm.
func (a *KubernetesDockerAdapter) RemoveVolume(ctx context.Context, name string) error {
	if _, err := a.InspectVolume(ctx, name); err != nil {
		return err
	}
	claim := sanitiseResourceName(name)

	// As in Docker, a volume a container uses, even a stopped one, stays.
	deployments, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return fmt.Errorf("unable to list deployments: %w", err)
	}
	var users []string
	for _, d := range deployments.Items {
		if slices.ContainsFunc(d.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool {
			return v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim
		}) {
			users = append(users, containerID(d.UID))
		}
	}
	if len(users) > 0 {
		return errdefs.Conflict(fmt.Errorf("remove %s: volume is in use - [%s]", name, strings.Join(users, ", ")))
	}

	if err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Delete(ctx, claim, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("unable to delete PVC %q: %w", name, err)
	}

	return nil
}

// pvcToVolume converts a PVC to the Docker volume it backs.
func pvcToVolume(pvc corev1.PersistentVolumeClaim) *volume.Volume {
	options := map[string]string{}
	if raw := pvc.Annotations[types.AnnotationDriverOpts]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &options)
	}
	return &volume.Volume{
		Name:       recordedName(pvc.ObjectMeta),
		Driver:     "d2k",
		Mountpoint: "/var/lib/d2k/volumes/" + pvc.Name,
		Labels:     recordedLabels(pvc.ObjectMeta),
		Options:    options,
		Scope:      "local",
		CreatedAt:  pvc.CreationTimestamp.UTC().Format(time.RFC3339),
	}
}

// LogNFSStorageClasses probes the cluster for nfs.csi.k8s.io StorageClasses
// at startup, logs the NFS server addresses d2k will support, and caches the
// server->storageClassName mapping for use by SwarmCreateService.
func (a *KubernetesDockerAdapter) LogNFSStorageClasses(ctx context.Context) {
	storageClasses, err := a.client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Warn().Str("context", "KubernetesAdapter").Err(err).Msg("Unable to list StorageClasses for NFS probe")
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
		a.cacheNFSStorageClass(server, sc.Name)
	}

	if len(found) == 0 {
		log.Info().Str("context", "KubernetesAdapter").Msg("No nfs.csi.k8s.io StorageClasses found — NFS volumes in Compose files will be rejected")
		return
	}

	for _, e := range found {
		log.Info().Str("context", "KubernetesAdapter").Any("storage_class", e.storageClass).Any("server", e.server).Msg("NFS volume support available")
	}
}

// nfsStorageClassForServer returns the StorageClass name for a given NFS server
// address, or ("", false) if none is cached.
func (a *KubernetesDockerAdapter) nfsStorageClassForServer(server string) (string, bool) {
	a.nfsMu.Lock()
	defer a.nfsMu.Unlock()
	sc, ok := a.nfsStorageClasses[server]
	return sc, ok
}

// anyNFSStorageClass returns the first cached NFS StorageClass name, or ("", false)
// if none are available. Used when server address is unknown at service create time.
func (a *KubernetesDockerAdapter) anyNFSStorageClass() (string, bool) {
	a.nfsMu.Lock()
	defer a.nfsMu.Unlock()
	for _, sc := range a.nfsStorageClasses {
		return sc, true
	}
	return "", false
}

// cacheNFSStorageClass records the StorageClass for an NFS server address.
func (a *KubernetesDockerAdapter) cacheNFSStorageClass(server, storageClass string) {
	a.nfsMu.Lock()
	defer a.nfsMu.Unlock()
	a.nfsStorageClasses[server] = storageClass
}
