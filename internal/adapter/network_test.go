package adapter

import (
	"context"
	"maps"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	k8stesting "k8s.io/client-go/testing"

	"github.com/portainer/d2k/internal/errdefs"
)

// Networks are recorded on the namespace, so they outlive d2k: a restarted
// d2k reports them exactly as they were created, joined or not.
func TestNetworksSurviveRestart(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	labels := map[string]string{
		"com.docker.compose.project": "netloss", "com.docker.compose.network": "front_end",
		"com.docker.compose.config-hash": strings.Repeat("f", 64), "team": "Payments Team",
	}
	if _, _, err := a.CreateNetwork(ctx, CreateNetworkOptions{Name: "netloss_front_end", Labels: labels, Internal: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.CreateNetwork(ctx, CreateNetworkOptions{Name: "netloss_front_end"}); !errdefs.IsConflict(err) {
		t.Fatalf("second create = %v, want conflict", err)
	}

	restarted := NewWithClient(client, testNamespace)
	n, err := restarted.InspectNetwork(ctx, "netloss_front_end")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range labels {
		if n.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, n.Labels[k], v)
		}
	}
	byID, err := restarted.InspectNetwork(ctx, n.ID)
	if err != nil || byID.Name != "netloss_front_end" {
		t.Errorf("inspect by ID = %+v, %v", byID, err)
	}
	if w := restarted.networkWarnings(ctx, []string{"netloss_front_end"}); len(w) != 1 || !strings.Contains(w[0], "internal") {
		t.Errorf("warnings after restart = %q", w)
	}
	list, _ := restarted.ListNetworks(ctx)
	if !containsNetwork(list, "netloss_front_end") {
		t.Errorf("list after restart misses the network")
	}

	// The record is an annotation on the namespace, and only that.
	ns, _ := client.CoreV1().Namespaces().Get(ctx, testNamespace, metav1.GetOptions{})
	keys := slicesOf(maps.Keys(ns.Annotations))
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "d2k.portainer.io/network-") || len(ns.Labels) != 0 {
		t.Errorf("namespace annotations = %q, labels = %v", keys, ns.Labels)
	}

	if err := restarted.RemoveNetwork(ctx, "netloss_front_end"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.InspectNetwork(ctx, "netloss_front_end"); !errdefs.IsNotFound(err) {
		t.Errorf("inspect after remove = %v, want not found", err)
	}
}

// A network created without a driver is a plain bridge network, scoped
// local, as Docker's own default is; one created with driver overlay is
// scoped swarm.
func TestNetworkRecordDriverAndScope(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()
	if _, _, err := a.CreateNetwork(ctx, CreateNetworkOptions{Name: "app_default"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.CreateNetwork(ctx, CreateNetworkOptions{Name: "app_overlay", Driver: "overlay"}); err != nil {
		t.Fatal(err)
	}

	bridge, err := a.InspectNetwork(ctx, "app_default")
	if err != nil {
		t.Fatal(err)
	}
	if bridge.Driver != "bridge" || bridge.Scope != "local" {
		t.Errorf("no driver: %+v, want driver bridge, scope local", bridge)
	}

	overlay, err := a.InspectNetwork(ctx, "app_overlay")
	if err != nil {
		t.Fatal(err)
	}
	if overlay.Driver != "overlay" || overlay.Scope != "swarm" {
		t.Errorf("driver overlay: %+v, want scope swarm", overlay)
	}
}

// Removing a network that was never created, or already removed, is not an
// error: d2k has always answered this way, as the Docker CLI's own
// best-effort `network rm` on a missing name expects.
func TestRemoveNetworkMissingIsNotAnError(t *testing.T) {
	a, _ := newTestAdapter(t)
	if err := a.RemoveNetwork(context.Background(), "nope"); err != nil {
		t.Fatalf("remove of a missing network = %v, want nil", err)
	}
}

// Docker refuses to remove a network a container is still joined to
// ("has active endpoints"). RemoveNetwork removed the record unconditionally,
// so a container's NetworkSettings kept listing a network that
// InspectNetwork then reported as gone. Found on a network built on the
// upgrade lane's pre-night estate, but the gap is in RemoveNetwork itself,
// not specific to pre-upgrade objects.
func TestRemoveNetworkRefusesWhileAContainerIsJoined(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()
	if _, _, err := a.CreateNetwork(ctx, CreateNetworkOptions{Name: "demo_back"}); err != nil {
		t.Fatal(err)
	}
	id := mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", Networks: []string{"demo_back"}})
	if err := a.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}

	if err := a.RemoveNetwork(ctx, "demo_back"); !errdefs.IsForbidden(err) {
		t.Fatalf("remove while a container is joined = %v, want forbidden (has active endpoints)", err)
	}
	if _, err := a.InspectNetwork(ctx, "demo_back"); err != nil {
		t.Fatalf("network removed despite a container still joined: inspect = %v", err)
	}

	// Once the container is gone, removal succeeds as normal.
	if err := a.RemoveContainer(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if err := a.RemoveNetwork(ctx, "demo_back"); err != nil {
		t.Fatalf("remove after the container left = %v, want nil", err)
	}
}

// Only a running container has an endpoint on its networks in Docker: a
// network that only created or stopped containers joined is removed. compose
// down removes the project's networks while a one-off container compose run
// left stopped is still on them, as it does not remove those containers.
func TestRemoveNetworkOfStoppedContainers(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()
	if _, _, err := a.CreateNetwork(ctx, CreateNetworkOptions{Name: "demo_default"}); err != nil {
		t.Fatal(err)
	}
	// compose run without --rm: a container that runs once, and has ended.
	job := mustCreate(t, a, RunOptions{Name: "demo-web-run-1", Image: "busybox", RestartPolicy: "no", Networks: []string{"demo_default"}})
	if err := a.StartContainer(ctx, job); err != nil {
		t.Fatal(err)
	}
	setStatus(t, a, podOf(t, a, "demo-web-run-1"), corev1.PodSucceeded, corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}})
	// A container that was stopped, and one never started.
	web := mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", Networks: []string{"demo_default"}})
	if err := a.StartContainer(ctx, web); err != nil {
		t.Fatal(err)
	}
	if err := a.StopContainer(ctx, web); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, a, RunOptions{Name: "idle", Image: "nginx", Networks: []string{"demo_default"}})

	if err := a.RemoveNetwork(ctx, "demo_default"); err != nil {
		t.Fatalf("remove of a network only stopped containers joined = %v, want nil", err)
	}
}

// Network inspect's Containers lists the same running containers that
// refuse RemoveNetwork (D2K-090/091): compose down reads it to warn
// "Resource is still in use" instead of calling NetworkRemove at all.
// A stopped or never-started container, which does not hold an endpoint,
// is left out, as RemoveNetwork leaves it out of its own check.
func TestInspectNetworkDetailListsRunningContainers(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()
	if _, _, err := a.CreateNetwork(ctx, CreateNetworkOptions{Name: "demo_default"}); err != nil {
		t.Fatal(err)
	}
	web := mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", Networks: []string{"demo_default"}})
	if err := a.StartContainer(ctx, web); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, a, RunOptions{Name: "idle", Image: "nginx", Networks: []string{"demo_default"}})

	detail, err := a.InspectNetworkDetail(ctx, "demo_default")
	if err != nil {
		t.Fatal(err)
	}
	containers, ok := detail["Containers"].(map[string]any)
	if !ok {
		t.Fatalf("Containers = %T, want map[string]any", detail["Containers"])
	}
	if len(containers) != 1 {
		t.Fatalf("Containers = %#v, want exactly the running container", containers)
	}
	webID, err := a.resolveDeploymentName(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	d, err := a.client.AppsV1().Deployments(testNamespace).Get(ctx, webID, metav1GetOptions())
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := containers[containerID(d.UID)].(map[string]any)
	if !ok {
		t.Fatalf("Containers entry = %#v, want the running container keyed by its ID", containers)
	}
	if entry["Name"] != "web" {
		t.Fatalf("Containers[...].Name = %v, want %q", entry["Name"], "web")
	}
}

func containsNetwork(list []NetworkSummary, name string) bool {
	for _, n := range list {
		if n.Name == name {
			return true
		}
	}
	return false
}

func slicesOf(seq func(func(string) bool)) []string {
	var out []string
	for k := range seq {
		out = append(out, k)
	}
	return out
}

// A namespace patch the API server rejects, as it does once the namespace's
// annotations pass 256 KiB, fails at once with the API server's reason: only
// a failed resourceVersion test means the namespace changed meanwhile.
func TestNetworkCreateRejectedByAPIServerIsNotRetried(t *testing.T) {
	a, client := newTestAdapter(t)
	patches := 0
	client.PrependReactor("patch", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		patches++
		return true, nil, apierrors.NewInvalid(schema.GroupKind{Kind: "Namespace"}, testNamespace, field.ErrorList{
			field.TooLong(field.NewPath("metadata", "annotations"), "", 262144),
		})
	})
	_, _, err := a.CreateNetwork(context.Background(), CreateNetworkOptions{Name: "web_default"})
	if patches != 1 {
		t.Errorf("patches = %d, want 1", patches)
	}
	if err == nil || errdefs.IsUnavailable(err) || !strings.Contains(err.Error(), "Too long") {
		t.Errorf("create = %v, want the API server's rejection", err)
	}
}

// A namespace that changed between d2k's read and its patch fails the
// patch's resourceVersion test, which the API server answers 422 with no
// details: the network is recorded on a retry.
func TestNetworkCreateRetriesWhenTheNamespaceChanged(t *testing.T) {
	a, client := newTestAdapter(t)
	patches := 0
	client.PrependReactor("patch", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		patches++
		if patches > 1 {
			return false, nil, nil
		}
		// Reactors run under the clientset's lock: change the namespace
		// through its tracker.
		gvr := corev1.SchemeGroupVersion.WithResource("namespaces")
		obj, err := client.Tracker().Get(gvr, "", testNamespace)
		if err != nil {
			return true, nil, err
		}
		ns := obj.(*corev1.Namespace).DeepCopy()
		ns.ResourceVersion = "2"
		if err := client.Tracker().Update(gvr, ns, ""); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewGenericServerResponse(http.StatusUnprocessableEntity, "", schema.GroupResource{}, "", "testing value /metadata/resourceVersion failed: test failed", 0, false)
	})
	if _, _, err := a.CreateNetwork(context.Background(), CreateNetworkOptions{Name: "web_default"}); err != nil {
		t.Fatalf("create = %v, want the network recorded on a retry", err)
	}
	if patches != 2 {
		t.Errorf("patches = %d, want 2", patches)
	}
	if _, err := a.InspectNetwork(context.Background(), "web_default"); err != nil {
		t.Errorf("inspect = %v", err)
	}
}
