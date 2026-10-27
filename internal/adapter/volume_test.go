package adapter

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/portainer/d2k/internal/errdefs"
)

func TestRemoveVolumeNotFound(t *testing.T) {
	a, _ := newTestAdapter(t)
	if err := a.RemoveVolume(context.Background(), "nope"); !errdefs.IsNotFound(err) {
		t.Fatalf("remove of a missing volume = %v, want not found", err)
	}
}

func TestCreateVolumeInvalidSize(t *testing.T) {
	a, _ := newTestAdapter(t)
	if _, err := a.CreateVolume(context.Background(), CreateVolumeOptions{Name: "data", SizeLimit: "not-a-size"}); !errdefs.IsInvalidParameter(err) {
		t.Fatalf("create with an invalid size = %v, want invalid parameter", err)
	}
}

func TestCreateVolumeMissingName(t *testing.T) {
	a, _ := newTestAdapter(t)
	if _, err := a.CreateVolume(context.Background(), CreateVolumeOptions{}); !errdefs.IsInvalidParameter(err) {
		t.Fatalf("create without a name = %v, want invalid parameter", err)
	}
}

func TestCreateNFSVolumeMissingAddr(t *testing.T) {
	a, _ := newTestAdapter(t)
	_, err := a.CreateVolume(context.Background(), CreateVolumeOptions{Name: "data", DriverOpts: map[string]string{"type": "nfs", "o": "nolock,soft,rw"}})
	if err == nil {
		t.Fatal("create NFS volume without addr= = nil, want an error")
	}
}

func TestCreateNFSVolumeNoMatchingStorageClass(t *testing.T) {
	a, _ := newTestAdapter(t)
	_, err := a.CreateVolume(context.Background(), CreateVolumeOptions{Name: "data", DriverOpts: map[string]string{"type": "nfs", "o": "addr=10.0.0.9"}})
	if err == nil {
		t.Fatal("create NFS volume with no matching StorageClass = nil, want an error")
	}
}

// A StorageClass whose provisioner isn't nfs.csi.k8s.io, or whose server
// doesn't match, is not a match even if it is the only StorageClass present.
func TestCreateNFSVolumeStorageClassMustMatch(t *testing.T) {
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "other"}, Provisioner: "other.csi.k8s.io",
		Parameters: map[string]string{"server": "10.0.0.9"}}
	a, _ := newTestAdapter(t, sc)
	if _, err := a.CreateVolume(context.Background(), CreateVolumeOptions{Name: "data", DriverOpts: map[string]string{"type": "nfs", "o": "addr=10.0.0.9"}}); err == nil {
		t.Fatal("create NFS volume against a non-NFS StorageClass = nil, want an error")
	}
}

func TestCreateNFSVolumeMatchingStorageClass(t *testing.T) {
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "nfs"}, Provisioner: nfsCsiProvisioner,
		Parameters: map[string]string{"server": "10.0.0.9"}}
	a, _ := newTestAdapter(t, sc)
	vol, err := a.CreateVolume(context.Background(), CreateVolumeOptions{Name: "data", DriverOpts: map[string]string{"type": "nfs", "o": "addr=10.0.0.9,nolock"}})
	if err != nil {
		t.Fatal(err)
	}
	if vol.Name != "data" {
		t.Fatalf("created = %+v", vol)
	}
	got, err := a.InspectVolume(context.Background(), "data")
	if err != nil {
		t.Fatal(err)
	}
	if got.Options["type"] != "nfs" {
		t.Fatalf("inspected = %+v, want the nfs driver option recorded", got)
	}
}

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
	if _, err := a.CreateVolume(ctx, CreateVolumeOptions{Name: "demo_data"}); !errdefs.IsConflict(err) || !strings.Contains(err.Error(), `"demo-data"`) {
		t.Fatalf("create of another name for the PVC = %v, want a conflict naming demo-data", err)
	}
}

// Recreating a volume while its old PVC is still terminating (Kubernetes
// deletion is not instant, unlike Docker's local driver) that never clears
// still gets a retryable conflict, not a hang: a container that mounted the
// dying PVC would be scheduled against a claim about to vanish for good,
// and never run. volumeTerminationTimeout is shortened so the test doesn't
// take the real 30s.
func TestCreateVolumeWhileOldOneIsTerminating(t *testing.T) {
	orig := volumeTerminationTimeout
	volumeTerminationTimeout = 50 * time.Millisecond
	defer func() { volumeTerminationTimeout = orig }()

	now := metav1.Now()
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data", Namespace: testNamespace,
		Labels:            managedLabels("data"),
		Annotations:       map[string]string{"d2k.portainer.io/docker-name": "data"},
		DeletionTimestamp: &now,
		Finalizers:        []string{"kubernetes.io/pvc-protection"},
	}}
	a, _ := newTestAdapter(t, pvc)
	if _, err := a.CreateVolume(context.Background(), CreateVolumeOptions{Name: "data"}); !errdefs.IsConflict(err) {
		t.Fatalf("create while the old PVC never clears = %v, want a retryable conflict, not success with the dying PVC", err)
	}
}

// Recreating a volume right after removing it (docker rm -f; docker volume
// rm; docker run -v) is the realistic case: the old PVC is terminating only
// because its pod hasn't finished shutting down yet, and clears on its own
// within the pod's termination grace period. createPVC must wait that out
// and succeed, not hand the caller a conflict Docker would never raise.
func TestCreateVolumeWaitsForTerminatingPVCThenSucceeds(t *testing.T) {
	now := metav1.Now()
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data", Namespace: testNamespace,
		Labels:            managedLabels("data"),
		Annotations:       map[string]string{"d2k.portainer.io/docker-name": "data"},
		DeletionTimestamp: &now,
		Finalizers:        []string{"kubernetes.io/pvc-protection"},
	}}
	a, client := newTestAdapter(t, pvc)
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = client.CoreV1().PersistentVolumeClaims(testNamespace).Delete(context.Background(), "data", metav1.DeleteOptions{})
	}()
	if _, err := a.CreateVolume(context.Background(), CreateVolumeOptions{Name: "data"}); err != nil {
		t.Fatalf("create once the old PVC finishes terminating = %v, want success", err)
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

// Two containers created at once on a volume whose old PVC is terminating,
// as compose up does for two services sharing a volume right after
// compose down -v, both wait for it to go; then only one create wins. The
// other must get the volume the first one created, as Docker returns an
// existing volume, not wait out the timeout and fail.
func TestCreateVolumeConcurrentlyWhileOldOneIsTerminating(t *testing.T) {
	orig := volumeTerminationTimeout
	volumeTerminationTimeout = 3 * time.Second
	defer func() { volumeTerminationTimeout = orig }()

	now := metav1.Now()
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data", Namespace: testNamespace,
		Labels:            managedLabels("data"),
		Annotations:       map[string]string{"d2k.portainer.io/docker-name": "data"},
		DeletionTimestamp: &now,
		Finalizers:        []string{"kubernetes.io/pvc-protection"},
	}}
	a, client := newTestAdapter(t, pvc)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = a.CreateVolume(context.Background(), CreateVolumeOptions{Name: "data"})
		}()
	}
	time.Sleep(200 * time.Millisecond)
	_ = client.CoreV1().PersistentVolumeClaims(testNamespace).Delete(context.Background(), "data", metav1.DeleteOptions{})
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("create %d once the old PVC is gone = %v, want the volume", i, err)
		}
	}
}
