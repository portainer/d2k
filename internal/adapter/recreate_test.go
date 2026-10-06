package adapter

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// recreate creates a container from another's inspect output, as
// Portainer's Recreate and Duplicate do, and returns the new Deployment's
// pod template.
func recreate(t *testing.T, a *KubernetesDockerAdapter, from, name string) corev1.PodSpec {
	t.Helper()
	ctx := context.Background()
	inspect, err := a.InspectContainer(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	opts, _ := RunOptionsFromRequest(name, container.CreateRequest{Config: inspect.Config, HostConfig: inspect.HostConfig})
	mustCreate(t, a, opts)
	d, err := a.client.AppsV1().Deployments(testNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d.Spec.Template.Spec
}

func TestRecreateFromInspectKeepsVolumes(t *testing.T) {
	a, client := newTestAdapter(t)
	opts, _ := RunOptionsFromRequest("db", container.CreateRequest{
		Config: &container.Config{Image: "postgres"},
		HostConfig: &container.HostConfig{
			Binds: []string{"pgdata:/var/lib/postgresql/data", "conf:/etc/app:ro"},
			Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: "shared", Target: "/srv",
				VolumeOptions: &mount.VolumeOptions{Subpath: "web"}}},
		},
	})
	mustCreate(t, a, opts)
	d, _ := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "db", metav1.GetOptions{})

	type volumeMount struct {
		claim, path, subPath string
		readOnly             bool
	}
	mounts := func(pod corev1.PodSpec) map[string]volumeMount {
		claims := map[string]string{}
		for _, v := range pod.Volumes {
			if v.PersistentVolumeClaim != nil {
				claims[v.Name] = v.PersistentVolumeClaim.ClaimName
			}
		}
		out := map[string]volumeMount{}
		for _, m := range pod.Containers[0].VolumeMounts {
			out[m.MountPath] = volumeMount{claims[m.Name], m.MountPath, m.SubPath, m.ReadOnly}
		}
		return out
	}
	want := mounts(d.Spec.Template.Spec)
	got := mounts(recreate(t, a, "db", "db2"))
	if len(got) != len(want) {
		t.Fatalf("recreated container mounts = %+v, want %+v", got, want)
	}
	for path, m := range want {
		if got[path] != m {
			t.Errorf("recreated mount at %s = %+v, want %+v", path, got[path], m)
		}
	}
}
