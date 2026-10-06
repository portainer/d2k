package adapter

import (
	"archive/tar"
	"bytes"
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/portainer/d2k/internal/errdefs"
)

type file struct {
	name, content string
	mode          int64
	kind          byte
}

func archive(t *testing.T, files ...file) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, f := range files {
		kind := f.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		if err := w.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.content)), Typeflag: kind}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(f.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}

// Compose writes secrets and configs from `environment` and `content` into
// the container before starting it, one archive per file.
func TestCopyToContainer(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	id, _, err := a.CreateContainer(ctx, RunOptions{Name: "web", Image: "nginx"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.CopyToContainer(ctx, id, "/", archive(t, file{name: "/run/secrets/pw", content: "s3cret", mode: 0o400})); err != nil {
		t.Fatal(err)
	}
	if err := a.CopyToContainer(ctx, "web", "/etc", archive(t, file{name: "app/app.conf", content: "a=1", mode: 0o444})); err != nil {
		t.Fatal(err)
	}
	// Compose uploads again before every start.
	if err := a.CopyToContainer(ctx, "web", "/", archive(t, file{name: "/run/secrets/pw", content: "changed", mode: 0o400})); err != nil {
		t.Fatal(err)
	}

	d, _ := client.AppsV1().Deployments(testNamespace).Get(ctx, "web", metav1.GetOptions{})
	pod := d.Spec.Template.Spec
	var source *corev1.SecretVolumeSource
	for _, v := range pod.Volumes {
		if v.Secret != nil {
			source = v.Secret
		}
	}
	if source == nil {
		t.Fatalf("volumes = %+v, want a Secret volume", pod.Volumes)
	}
	secret, err := client.CoreV1().Secrets(testNamespace).Get(ctx, source.SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].UID != d.UID {
		t.Errorf("owners = %+v, want the container's Deployment", secret.OwnerReferences)
	}

	files := map[string]string{}
	modes := map[string]int32{}
	for _, m := range pod.Containers[0].VolumeMounts {
		if !m.ReadOnly {
			t.Errorf("mount %s is writable", m.MountPath)
		}
		files[m.MountPath] = string(secret.Data[m.SubPath])
		for _, item := range source.Items {
			if item.Path == m.SubPath && item.Mode != nil {
				modes[m.MountPath] = *item.Mode
			}
		}
	}
	if len(files) != 2 || files["/run/secrets/pw"] != "changed" || files["/etc/app/app.conf"] != "a=1" {
		t.Errorf("files = %v", files)
	}
	if modes["/run/secrets/pw"] != 0o400 || modes["/etc/app/app.conf"] != 0o444 {
		t.Errorf("modes = %v", modes)
	}
}

func TestCopyToContainerRefuses(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()
	if _, _, err := a.CreateContainer(ctx, RunOptions{Name: "web", Image: "nginx"}); err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		files []file
		want  func(error) bool
	}{
		"a symlink":          {[]file{{name: "/link", kind: tar.TypeSymlink}}, errdefs.IsInvalidParameter},
		"more than a Secret": {[]file{{name: "/big", content: strings.Repeat("x", 1<<20+1)}}, errdefs.IsInvalidParameter},
	} {
		if err := a.CopyToContainer(ctx, "web", "/", archive(t, tt.files...)); !tt.want(err) {
			t.Errorf("copy of %s = %v", name, err)
		}
	}

	if err := a.CopyToContainer(ctx, "web", "/etc", archive(t, file{name: "../x", content: "x"})); !errdefs.IsInvalidParameter(err) {
		t.Errorf("copy of a path outside the destination = %v, want invalid parameter", err)
	}
	if err := a.CopyToContainer(ctx, "nope", "/", archive(t, file{name: "/x"})); !errdefs.IsNotFound(err) {
		t.Errorf("copy into a missing container = %v, want not found", err)
	}
	if err := a.StartContainer(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if err := a.CopyToContainer(ctx, "web", "/", archive(t, file{name: "/x", content: "x"})); !errdefs.IsNotImplemented(err) {
		t.Errorf("copy into a started container = %v, want not implemented", err)
	}
}

// The Secret of a container's copied files is the container's own: a
// Secret of that name that another workload owns is left alone, and one
// left by a removed container of the same name is replaced, not added to.
func TestCopyToContainerSecretIsTheContainers(t *testing.T) {
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "web-files", Namespace: testNamespace},
		Data: map[string][]byte{"password": []byte("theirs")}}
	stale := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "api-files", Namespace: testNamespace,
		Labels:          map[string]string{"d2k.portainer.io/managed-by": "d2k"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "api", UID: "removed-container"}}},
		Data: map[string][]byte{"file-old": []byte("old")}}
	a, client := newTestAdapter(t, foreign, stale)
	ctx := context.Background()

	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx"})
	if err := a.CopyToContainer(ctx, "web", "/", archive(t, file{name: "/run/secrets/pw", content: "s3cret", mode: 0o400})); !errdefs.IsConflict(err) {
		t.Errorf("copy with another workload's Secret web-files = %v, want conflict", err)
	}
	if s, _ := client.CoreV1().Secrets(testNamespace).Get(ctx, "web-files", metav1.GetOptions{}); len(s.Data) != 1 || string(s.Data["password"]) != "theirs" {
		t.Errorf("Secret web-files = %v, want it unchanged", s.Data)
	}

	mustCreate(t, a, RunOptions{Name: "api", Image: "nginx"})
	if err := a.CopyToContainer(ctx, "api", "/", archive(t, file{name: "/run/secrets/pw", content: "s3cret", mode: 0o400})); err != nil {
		t.Fatal(err)
	}
	d, _ := client.AppsV1().Deployments(testNamespace).Get(ctx, "api", metav1.GetOptions{})
	s, _ := client.CoreV1().Secrets(testNamespace).Get(ctx, "api-files", metav1.GetOptions{})
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != d.UID {
		t.Errorf("owners of api-files = %+v, want the new container's Deployment only: the old owner is gone and the garbage collector would delete it", s.OwnerReferences)
	}
	if _, ok := s.Data["file-old"]; ok || len(s.Data) != 1 {
		t.Errorf("api-files data keys = %d, want the new container's file only", len(s.Data))
	}
}
