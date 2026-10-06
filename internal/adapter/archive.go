package adapter

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/internal/types"
)

// maxCopiedBytes is the most a container's copied files may hold in total:
// the size limit of the Kubernetes Secret that stores them.
const maxCopiedBytes = 1 << 20

// copiedFilesVolume is the pod volume of a container's copied files.
const copiedFilesVolume = "copied-files"

// CopyToContainer writes the regular files of a tar archive into a container
// that has not been started, at their paths under dir, as `docker cp` and
// compose (for secrets and configs) do. The files are kept in a Secret owned
// by the container's Deployment and mounted read-only at their paths, with
// their modes; their owners are not kept.
func (a *KubernetesDockerAdapter) CopyToContainer(ctx context.Context, nameOrID, dir string, archive io.Reader) error {
	name, err := a.resolveDeploymentName(ctx, nameOrID)
	if err != nil {
		return err
	}
	d, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, name, metav1GetOptions())
	if err != nil {
		return fmt.Errorf("unable to get deployment %q: %w", name, err)
	}
	started := replicasOf(d) > 0
	if runsAsPod(*d) {
		pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, podsOf(name))
		if err != nil {
			return fmt.Errorf("unable to list the pods of %q: %w", name, err)
		}
		started = slices.ContainsFunc(pods.Items, func(p corev1.Pod) bool { return p.DeletionTimestamp == nil && !podEnded(p) })
	}
	if started {
		return errdefs.NotImplemented(errors.New("copying files into a started container is not supported by d2k: copy them before starting it"))
	}

	files, err := readArchive(dir, archive)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}

	secret, err := a.storeCopiedFiles(ctx, d, files)
	if err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		d, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, name, metav1GetOptions())
		if err != nil {
			return err
		}
		mountCopiedFiles(&d.Spec.Template.Spec, secret, files)
		_, err = a.client.AppsV1().Deployments(a.namespace).Update(ctx, d, metav1.UpdateOptions{})
		return err
	})
}

// copiedFile is a file of an archive, at its path in the container.
type copiedFile struct {
	target  string
	mode    int32
	content []byte
}

// key is the file's key in the Secret, which allows only [-._a-zA-Z0-9].
func (f copiedFile) key() string {
	sum := sha256.Sum256([]byte(f.target))
	return "file-" + hex.EncodeToString(sum[:8])
}

// readArchive reads the regular files of a tar archive, which may be
// gzip-compressed, as the Engine API allows. Directories are implied by the
// files' paths; other entries cannot be stored in a Secret.
func readArchive(dir string, archive io.Reader) ([]copiedFile, error) {
	r := bufio.NewReader(archive)
	if magic, _ := r.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, errdefs.InvalidParameter(fmt.Errorf("invalid archive: %w", err))
		}
		defer func() { _ = gz.Close() }()
		archive = gz
	} else {
		archive = r
	}

	dir = path.Clean("/" + dir)
	var files []copiedFile
	tr := tar.NewReader(archive)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, errdefs.InvalidParameter(fmt.Errorf("invalid archive: %w", err))
		}
		target := path.Join(dir, h.Name)
		if !strings.HasPrefix(path.Join(dir, h.Name, "x"), strings.TrimSuffix(dir, "/")+"/") {
			return nil, errdefs.InvalidParameter(fmt.Errorf("invalid archive: %q is outside %s", h.Name, dir))
		}
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return nil, errdefs.InvalidParameter(fmt.Errorf("%q is not a regular file: d2k copies regular files only", h.Name))
		}
		content, err := io.ReadAll(io.LimitReader(tr, maxCopiedBytes+1))
		if err != nil {
			return nil, errdefs.InvalidParameter(fmt.Errorf("invalid archive: %w", err))
		}
		files = append(files, copiedFile{target: target, mode: int32(h.Mode & 0o777), content: content})
	}
}

// storeCopiedFiles adds files to the Secret of the container's copied files.
func (a *KubernetesDockerAdapter) storeCopiedFiles(ctx context.Context, d *appsv1.Deployment, files []copiedFile) (string, error) {
	name := d.Name + "-files"
	secrets := a.client.CoreV1().Secrets(a.namespace)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secret, err := secrets.Get(ctx, name, metav1GetOptions())
		create := apierrors.IsNotFound(err)
		switch {
		case create:
			secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: a.namespace,
				Labels:          map[string]string{types.LabelManagedBy: types.LabelManagedByValue},
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: d.Name, UID: d.UID}},
			}}
		case err != nil:
			return err
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		for _, f := range files {
			secret.Data[f.key()] = f.content
		}
		size := 0
		for _, v := range secret.Data {
			size += len(v)
		}
		if size > maxCopiedBytes {
			return errdefs.InvalidParameter(fmt.Errorf("files copied into a container are limited to 1 MiB in total by d2k (the size of a Kubernetes Secret)"))
		}
		if create {
			_, err = secrets.Create(ctx, secret, metav1.CreateOptions{})
		} else {
			_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
		}
		return err
	})
	if err != nil && !errdefs.IsInvalidParameter(err) {
		err = fmt.Errorf("unable to store the copied files: %w", err)
	}
	return name, err
}

// mountCopiedFiles mounts each file of the Secret read-only at its path.
func mountCopiedFiles(pod *corev1.PodSpec, secret string, files []copiedFile) {
	i := slices.IndexFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == copiedFilesVolume })
	if i < 0 {
		pod.Volumes = append(pod.Volumes, corev1.Volume{Name: copiedFilesVolume, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: secret},
		}})
		i = len(pod.Volumes) - 1
	}
	source := pod.Volumes[i].Secret
	c := &pod.Containers[0]
	for _, f := range files {
		mode := f.mode
		item := corev1.KeyToPath{Key: f.key(), Path: f.key(), Mode: &mode}
		if j := slices.IndexFunc(source.Items, func(k corev1.KeyToPath) bool { return k.Key == item.Key }); j >= 0 {
			source.Items[j] = item
		} else {
			source.Items = append(source.Items, item)
		}
		if !slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == f.target }) {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: copiedFilesVolume, MountPath: f.target, SubPath: f.key(), ReadOnly: true})
		}
	}
}
