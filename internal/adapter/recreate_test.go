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

func TestRecreateFromInspectKeepsSecuritySettings(t *testing.T) {
	a, client := newTestAdapter(t)
	opts, _ := RunOptionsFromRequest("sec", container.CreateRequest{
		Config: &container.Config{Image: "alpine", User: "1000:1000"},
		HostConfig: &container.HostConfig{
			CapAdd: []string{"NET_ADMIN"}, CapDrop: []string{"MKNOD"},
			Privileged: true, ReadonlyRootfs: true,
			GroupAdd: []string{"1001", "1002"},
			Sysctls:  map[string]string{"net.ipv4.ip_forward": "1"},
			SecurityOpt: []string{
				"no-new-privileges",
				"seccomp=unconfined",
				"label=user:system_u",
			},
		},
	})
	mustCreate(t, a, opts)
	d, _ := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "sec", metav1.GetOptions{})
	wantSC, wantPodSC := d.Spec.Template.Spec.Containers[0].SecurityContext, d.Spec.Template.Spec.SecurityContext

	got := recreate(t, a, "sec", "sec2")
	if !equality.Semantic.DeepEqual(got.Containers[0].SecurityContext, wantSC) {
		t.Errorf("recreated container SecurityContext = %+v, want %+v", got.Containers[0].SecurityContext, wantSC)
	}
	if !equality.Semantic.DeepEqual(got.SecurityContext, wantPodSC) {
		t.Errorf("recreated pod SecurityContext = %+v, want %+v", got.SecurityContext, wantPodSC)
	}
}

func TestRecreateFromInspectKeepsNetworkingSettings(t *testing.T) {
	a, client := newTestAdapter(t)
	opts, _ := RunOptionsFromRequest("net", container.CreateRequest{
		Config: &container.Config{Image: "alpine"},
		HostConfig: &container.HostConfig{
			ExtraHosts: []string{"foo.example:10.0.0.5", "bar.example:10.0.0.5"},
			DNSSearch:  []string{"svc.cluster.local"},
			DNSOptions: []string{"ndots:2", "timeout:1"},
		},
	})
	mustCreate(t, a, opts)
	d, _ := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "net", metav1.GetOptions{})
	wantAliases, wantDNS := d.Spec.Template.Spec.HostAliases, d.Spec.Template.Spec.DNSConfig

	got := recreate(t, a, "net", "net2")
	if !equality.Semantic.DeepEqual(got.HostAliases, wantAliases) {
		t.Errorf("recreated pod HostAliases = %+v, want %+v", got.HostAliases, wantAliases)
	}
	if !equality.Semantic.DeepEqual(got.DNSConfig, wantDNS) {
		t.Errorf("recreated pod DNSConfig = %+v, want %+v", got.DNSConfig, wantDNS)
	}
}

func TestRecreateFromInspectKeepsProcessSettings(t *testing.T) {
	a, client := newTestAdapter(t)
	grace := 42
	opts, _ := RunOptionsFromRequest("proc", container.CreateRequest{
		Config: &container.Config{
			Image: "alpine", WorkingDir: "/srv", Hostname: "myhost", StopTimeout: &grace,
		},
	})
	mustCreate(t, a, opts)
	d, _ := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "proc", metav1.GetOptions{})
	want := d.Spec.Template.Spec

	got := recreate(t, a, "proc", "proc2")
	if got.Containers[0].WorkingDir != want.Containers[0].WorkingDir {
		t.Errorf("recreated WorkingDir = %q, want %q", got.Containers[0].WorkingDir, want.Containers[0].WorkingDir)
	}
	if got.Hostname != want.Hostname {
		t.Errorf("recreated Hostname = %q, want %q", got.Hostname, want.Hostname)
	}
	if !equality.Semantic.DeepEqual(got.TerminationGracePeriodSeconds, want.TerminationGracePeriodSeconds) {
		t.Errorf("recreated TerminationGracePeriodSeconds = %v, want %v", got.TerminationGracePeriodSeconds, want.TerminationGracePeriodSeconds)
	}
}

func TestRecreateFromInspectKeepsStorageSettings(t *testing.T) {
	a, client := newTestAdapter(t)
	opts, _ := RunOptionsFromRequest("store", container.CreateRequest{
		Config: &container.Config{Image: "alpine"},
		HostConfig: &container.HostConfig{
			ShmSize: 128 << 20,
			Tmpfs:   map[string]string{"/cache": "size=64m"},
		},
	})
	mustCreate(t, a, opts)
	d, _ := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "store", metav1.GetOptions{})
	want := d.Spec.Template.Spec

	type tmpfs struct {
		Target string
		Size   int64
	}
	tmpfsMounts := func(pod corev1.PodSpec) map[string]tmpfs {
		volumes := map[string]*corev1.EmptyDirVolumeSource{}
		for _, v := range pod.Volumes {
			if v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory {
				volumes[v.Name] = v.EmptyDir
			}
		}
		out := map[string]tmpfs{}
		for _, m := range pod.Containers[0].VolumeMounts {
			if v, ok := volumes[m.Name]; ok {
				var size int64
				if v.SizeLimit != nil {
					size = v.SizeLimit.Value()
				}
				out[m.MountPath] = tmpfs{m.MountPath, size}
			}
		}
		return out
	}
	wantMounts, gotMounts := tmpfsMounts(want), tmpfsMounts(recreate(t, a, "store", "store2"))
	if !equality.Semantic.DeepEqual(gotMounts, wantMounts) {
		t.Errorf("recreated tmpfs mounts = %+v, want %+v", gotMounts, wantMounts)
	}
}
