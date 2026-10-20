package adapter

import (
	"context"
	"maps"
	"testing"

	"github.com/moby/moby/api/types/container"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/portainer/d2k/internal/types"
)

// legacyDeployment is a container as d2k 1.2.x created it: d2k's labels
// and the Docker labels whose values Kubernetes accepts mixed together as
// Kubernetes labels, and no recorded Docker name, labels or start time.
func legacyDeployment(name string, replicas int32, dockerLabels map[string]string) *appsv1.Deployment {
	labels := map[string]string{
		"app": name, types.LabelManagedBy: types.LabelManagedByValue, types.LabelWorkloadName: name, types.LabelServiceType: "none",
	}
	maps.Copy(labels, dockerLabels)
	d := deployment(name, "uid-"+name, labels)
	d.CreationTimestamp = metav1.NewTime(now.Add(-24 * 60 * 60 * 1e9))
	d.Annotations = map[string]string{types.AnnotationImageRef: "redis:7-alpine", types.AnnotationPortMappings: ""}
	d.Spec.Replicas = &replicas
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: name, Image: "redis:7-alpine"}}
	return d
}

// Upgrading from d2k 1.2.x: its compose containers lacked the
// com.docker.compose.config-hash label (its 64-character value exceeds a
// Kubernetes label value), so docker compose ls and ps listed nothing;
// d2k's own labels showed as Docker labels; a stopped container showed as
// created. The migration records what d2k reads now, once.
func TestMigrateLegacyObjects(t *testing.T) {
	compose := map[string]string{"com.docker.compose.project": "legproj", "com.docker.compose.service": "cache", "com.docker.compose.oneoff": "False"}
	cache := legacyDeployment("legproj-cache-1", 1, compose)
	plain := legacyDeployment("plain", 0, nil)
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "legproj-cachedata", Namespace: testNamespace, Labels: map[string]string{
		"app": "legproj-cachedata", types.LabelManagedBy: types.LabelManagedByValue, types.LabelWorkloadName: "legproj-cachedata",
		"com.docker.compose.project": "legproj", "com.docker.compose.volume": "cachedata",
	}}}
	a, client := newTestAdapter(t, cache, plain, pvc)
	ctx := context.Background()

	if err := a.MigrateLegacyObjects(ctx, false); err != nil {
		t.Fatal(err)
	}

	want := maps.Clone(compose)
	want["com.docker.compose.config-hash"] = "d2k-migrated"
	ctrs, err := a.ListContainers(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range ctrs {
		switch c.Names[0] {
		case "/legproj-cache-1":
			if !maps.Equal(c.Labels, want) {
				t.Errorf("compose container labels = %v, want %v", c.Labels, want)
			}
		case "/plain":
			if len(c.Labels) != 0 {
				t.Errorf("plain container labels = %v, want none: d2k's own labels aren't Docker labels", c.Labels)
			}
		}
	}
	if ctr, err := a.InspectContainer(ctx, "plain"); err != nil || ctr.State.Status != container.StateExited {
		t.Errorf("stopped legacy container state = %+v, %v, want exited", ctr, err)
	}
	vol, err := a.InspectVolume(ctx, "legproj-cachedata")
	if err != nil {
		t.Fatal(err)
	}
	if wantVol := map[string]string{"com.docker.compose.project": "legproj", "com.docker.compose.volume": "cachedata"}; !maps.Equal(vol.Labels, wantVol) {
		t.Errorf("volume labels = %v, want %v", vol.Labels, wantVol)
	}

	// Once: a second run changes nothing.
	before, _ := client.AppsV1().Deployments(testNamespace).Get(ctx, "legproj-cache-1", metav1.GetOptions{})
	if err := a.MigrateLegacyObjects(ctx, false); err != nil {
		t.Fatal(err)
	}
	after, _ := client.AppsV1().Deployments(testNamespace).Get(ctx, "legproj-cache-1", metav1.GetOptions{})
	if !maps.Equal(before.Annotations, after.Annotations) {
		t.Errorf("second migration changed %v to %v", before.Annotations, after.Annotations)
	}
}

// Containers d2k creates now already record everything: the migration
// leaves them alone.
func TestMigrateLeavesCurrentObjectsAlone(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", Labels: map[string]string{"app": "mine"}})
	before, _ := client.AppsV1().Deployments(testNamespace).Get(ctx, "web", metav1.GetOptions{})
	if err := a.MigrateLegacyObjects(ctx, false); err != nil {
		t.Fatal(err)
	}
	after, _ := client.AppsV1().Deployments(testNamespace).Get(ctx, "web", metav1.GetOptions{})
	if !maps.Equal(before.Annotations, after.Annotations) {
		t.Errorf("migration changed a current container: %v to %v", before.Annotations, after.Annotations)
	}
}
