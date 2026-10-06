package adapter

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
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

func TestRecreateFromInspectKeepsResources(t *testing.T) {
	a, client := newTestAdapter(t)
	opts, _ := RunOptionsFromRequest("api", container.CreateRequest{
		Config: &container.Config{Image: "nginx"},
		HostConfig: &container.HostConfig{Resources: container.Resources{
			Memory: 256 << 20, MemoryReservation: 128 << 20, NanoCPUs: 500_000_000, CPUShares: 512,
		}},
	})
	mustCreate(t, a, opts)
	d, _ := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "api", metav1.GetOptions{})
	want := d.Spec.Template.Spec.Containers[0].Resources

	inspect, _ := a.InspectContainer(context.Background(), "api")
	if r := inspect.HostConfig.Resources; r.Memory != 256<<20 || r.MemoryReservation != 128<<20 || r.NanoCPUs != 500_000_000 || r.CPUShares != 512 {
		t.Errorf("inspect HostConfig: Memory %d, MemoryReservation %d, NanoCpus %d, CpuShares %d; want the values the container was created with",
			r.Memory, r.MemoryReservation, r.NanoCPUs, r.CPUShares)
	}
	if got := recreate(t, a, "api", "api2").Containers[0].Resources; !equality.Semantic.DeepEqual(got, want) {
		t.Errorf("recreated container resources = %v, want %v", got, want)
	}
}
