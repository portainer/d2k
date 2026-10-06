package adapter

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/moby/moby/api/types/events"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"

	"github.com/portainer/d2k/internal/types"
)

func eventDeployment(replicas int32, annotations map[string]string) *appsv1.Deployment {
	d := deployment("web-k8s", "11111111-0000-4000-8000-000000000001", managed())
	d.Spec.Replicas = &replicas
	d.Annotations = map[string]string{
		types.AnnotationDockerName:   "web",
		types.AnnotationImageRef:     "nginx:alpine",
		types.AnnotationDockerLabels: `{"com.docker.compose.project":"demo"}`,
	}
	for k, v := range annotations {
		d.Annotations[k] = v
	}
	return d
}

func actions(msgs []events.Message) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, string(m.Type)+" "+string(m.Action))
	}
	return out
}

func TestDeploymentEvents(t *testing.T) {
	known := map[string]*appsv1.Deployment{}
	now := time.Now()

	created := deploymentEvents(known, watch.Added, eventDeployment(0, nil), now)
	if got := actions(created); !slices.Equal(got, []string{"container create"}) {
		t.Fatalf("added = %q", got)
	}
	m := created[0]
	if m.Actor.ID != containerID("11111111-0000-4000-8000-000000000001") || m.Actor.Attributes["name"] != "web" ||
		m.Actor.Attributes["image"] != "nginx:alpine" || m.Actor.Attributes["com.docker.compose.project"] != "demo" || m.Scope != "local" {
		t.Fatalf("create event = %+v", m)
	}

	for _, tt := range []struct {
		name string
		next *appsv1.Deployment
		want []string
	}{
		{"start", eventDeployment(1, nil), []string{"container start"}},
		{"unrelated change", eventDeployment(1, map[string]string{"x": "y"}), nil},
		{"stop", eventDeployment(0, nil), []string{"container stop"}},
		{"rename", eventDeployment(0, map[string]string{types.AnnotationDockerName: "web2"}), []string{"container rename"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := actions(deploymentEvents(known, watch.Modified, tt.next, now)); !slices.Equal(got, tt.want) {
				t.Fatalf("events = %q, want %q", got, tt.want)
			}
		})
	}
	renamed := deploymentEvents(known, watch.Modified, eventDeployment(0, map[string]string{types.AnnotationDockerName: "web3"}), now)
	if renamed[0].Actor.Attributes["oldName"] != "/web2" || renamed[0].Actor.Attributes["name"] != "web3" {
		t.Fatalf("rename attributes = %v", renamed[0].Actor.Attributes)
	}
	if got := actions(deploymentEvents(known, watch.Deleted, eventDeployment(0, nil), now)); !slices.Equal(got, []string{"container destroy"}) {
		t.Fatalf("deleted = %q", got)
	}
}

func TestPodEventsReportDeaths(t *testing.T) {
	known := map[string]*appsv1.Deployment{"web-k8s": eventDeployment(1, nil)}
	restarts := map[string]int32{}
	pod := func(restarts int32, exit int32) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-k8s-abc", Labels: map[string]string{"app": "web-k8s"}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				RestartCount:         restarts,
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit}},
			}}},
		}
	}
	if got := podEvents(known, restarts, map[string]int32{}, watch.Added, pod(0, 0), time.Now()); got != nil {
		t.Fatalf("new pod = %q, want no event", actions(got))
	}
	died := podEvents(known, restarts, map[string]int32{}, watch.Modified, pod(1, 137), time.Now())
	if got := actions(died); !slices.Equal(got, []string{"container die"}) || died[0].Actor.Attributes["exitCode"] != "137" {
		t.Fatalf("restart = %q %v", got, died)
	}
	if got := podEvents(known, restarts, map[string]int32{}, watch.Modified, pod(1, 137), time.Now()); got != nil {
		t.Fatalf("same restart count = %q, want no event", actions(got))
	}
}

func TestEventHub(t *testing.T) {
	var hub eventHub
	for i := range eventHistorySize + 10 {
		hub.publish(events.Message{Action: events.Action("e"), TimeNano: int64(i)})
	}
	history, live, cancel := hub.subscribe()
	if len(history) != eventHistorySize || history[0].TimeNano != 10 {
		t.Fatalf("history has %d events from %d, want %d from 10", len(history), history[0].TimeNano, eventHistorySize)
	}
	hub.publish(events.Message{Action: "live"})
	if m := <-live; m.Action != "live" {
		t.Fatalf("live = %+v", m)
	}
	cancel()
	cancel()                                     // idempotent
	hub.publish(events.Message{Action: "after"}) // no subscriber, must not block
}

// A container that exits while the namespace's watches are restarting, as
// Kubernetes ends them every 30 to 60 minutes, still gets its die event:
// docker wait and an attached docker run wait for it.
func TestEventsReportDeathsAcrossWatchRestarts(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, _, err := a.CreateContainer(ctx, RunOptions{Name: "job", Image: "busybox", RestartPolicy: "no"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	p := podOf(t, a, "job")
	setStatus(t, a, p, corev1.PodRunning, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})

	podWatches := make(chan *watch.FakeWatcher, 4)
	client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		w := watch.NewFake()
		podWatches <- w
		return true, w, nil
	})
	_, live, unsubscribe := a.SubscribeEvents()
	defer unsubscribe()
	go a.RecordEvents(ctx)

	var first *watch.FakeWatcher
	select {
	case first = <-podWatches:
	case <-time.After(5 * time.Second):
		t.Fatal("no pod watch")
	}
	// The container exits, and the watch ends before reporting it.
	p = podOf(t, a, "job")
	setStatus(t, a, p, corev1.PodFailed, corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}})
	first.Stop()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-live:
			if m.Action == events.ActionDie && m.Actor.ID == id {
				if m.Actor.Attributes["exitCode"] != "3" {
					t.Fatalf("die event exit code = %q, want 3", m.Actor.Attributes["exitCode"])
				}
				return
			}
		case <-deadline:
			t.Fatal("no die event for the container that exited while the watch restarted")
		}
	}
}

// A run-once container whose pod is force-deleted or lost with its node
// gets a die event too, so docker wait and an attached docker run don't
// hang forever even though the kubelet never reported it terminating
// (D2K-035).
func TestEventsReportALostPodKilled(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, _, err := a.CreateContainer(ctx, RunOptions{Name: "job", Image: "busybox", RestartPolicy: "no"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	setStatus(t, a, podOf(t, a, "job"), corev1.PodRunning, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})

	_, live, unsubscribe := a.SubscribeEvents()
	defer unsubscribe()
	go a.RecordEvents(ctx)

	// Give the first watch a moment to start before force-deleting the pod
	// out from under it, as kubectl delete --force --grace-period=0 does.
	time.Sleep(200 * time.Millisecond)
	if err := client.CoreV1().Pods(testNamespace).Delete(ctx, podOf(t, a, "job").Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-live:
			if m.Action == events.ActionDie && m.Actor.ID == id {
				if m.Actor.Attributes["exitCode"] != "137" {
					t.Fatalf("die event exit code = %q, want 137", m.Actor.Attributes["exitCode"])
				}
				return
			}
		case <-deadline:
			t.Fatal("no die event for the container whose pod was lost")
		}
	}
}

// As the Docker daemon does when it starts, d2k removes the AutoRemove
// containers whose run ended while it was not watching; one that runs, or
// was never started, stays.
func TestEventsRemoveRunsThatEndedBeforeTheWatch(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, name := range []string{"ended", "running", "created"} {
		mustCreate(t, a, RunOptions{Name: name, Image: "busybox", RestartPolicy: "no", AutoRemove: true})
	}
	for _, name := range []string{"ended", "running"} {
		if err := a.StartContainer(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	setStatus(t, a, podOf(t, a, "ended"), corev1.PodFailed, corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}})
	setStatus(t, a, podOf(t, a, "running"), corev1.PodRunning, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})

	go a.RecordEvents(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := a.InspectContainer(ctx, "ended"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the container whose run ended before d2k watched was not removed")
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, name := range []string{"running", "created"} {
		if _, err := a.InspectContainer(ctx, name); err != nil {
			t.Errorf("inspect %s = %v, want it kept", name, err)
		}
	}
}
