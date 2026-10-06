package adapter

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/portainer/d2k/internal/errdefs"
)

func TestVolumeRoundTrip(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()
	labels := map[string]string{
		"com.docker.compose.project": "demo",
		"note":                       "a value longer than the sixty-three characters Kubernetes allows in a label value",
	}
	created, err := a.CreateVolume(ctx, CreateVolumeOptions{Name: "demo_data", Labels: labels, DriverOpts: map[string]string{"size": "2Gi"}})
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "demo_data" {
		t.Fatalf("created = %+v", created)
	}
	got, err := a.InspectVolume(ctx, "demo_data")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "demo_data" || got.Scope != "local" || !maps.Equal(got.Labels, labels) || got.Options["size"] != "2Gi" {
		t.Fatalf("volume = %+v", got)
	}

	// Creating an existing volume returns it, as in Docker.
	again, err := a.CreateVolume(ctx, CreateVolumeOptions{Name: "demo_data"})
	if err != nil || again.Name != "demo_data" {
		t.Fatalf("second create = %+v, %v", again, err)
	}

	list, _ := a.ListVolumes(ctx)
	if len(list) != 1 || list[0].Name != "demo_data" || list[0].Options == nil {
		t.Fatalf("list = %+v", list)
	}
}

func TestCreateVolumeRefusesUnmanagedPVC(t *testing.T) {
	a, _ := newTestAdapter(t, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "db-data", Namespace: testNamespace}})
	if _, err := a.CreateVolume(context.Background(), CreateVolumeOptions{Name: "db-data"}); !errdefs.IsConflict(err) {
		t.Fatalf("create over an unmanaged PVC = %v, want conflict", err)
	}
}

func TestVolumeNamesSharingAPVCName(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()
	if _, err := a.CreateVolume(ctx, CreateVolumeOptions{Name: "demo-data"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.InspectVolume(ctx, "demo_data"); !errdefs.IsNotFound(err) {
		t.Fatalf("inspect of another name for the PVC = %v, want not found", err)
	}
	if _, err := a.CreateVolume(ctx, CreateVolumeOptions{Name: "demo_data"}); !errdefs.IsConflict(err) {
		t.Fatalf("create of another name for the PVC = %v, want conflict", err)
	}
}

func TestCreateVolumeRefusesOtherDrivers(t *testing.T) {
	a, _ := newTestAdapter(t)
	if _, err := a.CreateVolume(context.Background(), CreateVolumeOptions{Name: "data", Driver: "rexray"}); !errdefs.IsInvalidParameter(err) {
		t.Fatalf("create with driver rexray = %v, want invalid parameter", err)
	}
	for _, driver := range []string{"", "local"} {
		if _, err := a.CreateVolume(context.Background(), CreateVolumeOptions{Name: "data" + driver, Driver: driver}); err != nil {
			t.Fatalf("create with driver %q = %v", driver, err)
		}
	}
}

func TestRemoveVolumeInUse(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()
	if _, _, err := a.CreateContainer(ctx, RunOptions{Name: "db", Image: "postgres", Binds: []string{"db_data:/var/lib/postgresql/data"}}); err != nil {
		t.Fatal(err)
	}
	// As in Docker, a volume used by a container, even a stopped one, stays.
	if err := a.RemoveVolume(ctx, "db_data"); !errdefs.IsConflict(err) {
		t.Fatalf("remove of a volume in use = %v, want conflict", err)
	}
	if _, err := a.InspectVolume(ctx, "db_data"); err != nil {
		t.Fatalf("volume after refused remove: %v", err)
	}
	if err := a.RemoveContainer(ctx, "db"); err != nil {
		t.Fatal(err)
	}
	if err := a.RemoveVolume(ctx, "db_data"); err != nil {
		t.Fatalf("remove of a volume no longer in use = %v", err)
	}
}

// Several clients, or compose projects, create NFS volumes at once. The
// StorageClass was not there when d2k started. Run with -race.
func TestConcurrentNFSVolumes(t *testing.T) {
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "nfs"}, Provisioner: nfsCsiProvisioner,
		Parameters: map[string]string{"server": "10.0.0.9"}}
	a, _ := newTestAdapter(t, sc)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			opts := CreateVolumeOptions{Name: fmt.Sprintf("data%d", i), DriverOpts: map[string]string{"type": "nfs", "o": "addr=10.0.0.9"}}
			if _, err := a.CreateVolume(context.Background(), opts); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
