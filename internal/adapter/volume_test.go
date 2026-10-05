package adapter

import (
	"context"
	"maps"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
