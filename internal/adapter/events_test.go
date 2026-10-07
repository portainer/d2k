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
	terminations := map[string]int32{}
	pod := func(restarts int32, exit int32) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-k8s-abc", Labels: map[string]string{"app": "web-k8s"}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				RestartCount:         restarts,
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit}},
			}}},
		}
	}
	podApp := map[string]string{}
	if got := podEvents(known, terminations, podApp, watch.Added, pod(0, 0), time.Now()); got != nil {
		t.Fatalf("new pod = %q, want no event", actions(got))
	}
	died := podEvents(known, terminations, podApp, watch.Modified, pod(1, 137), time.Now())
	if got := actions(died); !slices.Equal(got, []string{"container die"}) || died[0].Actor.Attributes["exitCode"] != "137" {
		t.Fatalf("restart = %q %v", got, died)
	}
	if got := podEvents(known, terminations, podApp, watch.Modified, pod(1, 137), time.Now()); got != nil {
		t.Fatalf("same restart count = %q, want no event", actions(got))
	}
}

// A run-once container's pod that is force-deleted or lost with its node
// gets no termination reported by the kubelet, so it never shows up as a
// die through the normal Terminated-status path. podEvents still reports it
// killed, once, so docker wait and compose depends_on don't hang (D2K-035).
func TestPodEventsReportsALostPodKilled(t *testing.T) {
	d := eventDeployment(0, map[string]string{types.AnnotationRestartPolicy: "no", types.AnnotationStartedAt: "2026-01-01T00:00:00Z"})
	known := map[string]*appsv1.Deployment{"web-k8s": d}
	terminations, podApp := map[string]int32{}, map[string]string{}
	lost := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-k8s-abc", Labels: map[string]string{"app": "web-k8s"}}}

	if got := podEvents(known, terminations, podApp, watch.Added, lost, time.Now()); got != nil {
		t.Fatalf("added = %q, want no event", actions(got))
	}
	died := podEvents(known, terminations, podApp, watch.Deleted, lost, time.Now())
	if got := actions(died); !slices.Equal(got, []string{"container die"}) || died[0].Actor.Attributes["exitCode"] != "137" {
		t.Fatalf("lost pod = %q %v, want a single die with exit code 137", got, died)
	}

	// A pod d2k itself stopped (AnnotationStoppedAt recorded) is not reported
	// killed again when it is deleted.
	stopped := eventDeployment(0, map[string]string{types.AnnotationRestartPolicy: "no", types.AnnotationStartedAt: "2026-01-01T00:00:00Z", types.AnnotationStoppedAt: "2026-01-01T00:01:00Z"})
	known["web-k8s"] = stopped
	own := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-k8s-def", Labels: map[string]string{"app": "web-k8s"}}}
	if got := podEvents(known, terminations, podApp, watch.Deleted, own, time.Now()); got != nil {
		t.Fatalf("delete of a pod d2k recorded stopping = %q, want no event", actions(got))
	}

	// A pod whose termination was already reported (and so already died) is
	// not reported killed again when it is finally deleted.
	known["web-k8s"] = d
	terminated := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-k8s-ghi", Labels: map[string]string{"app": "web-k8s"}},
		Spec:   corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}}}}}}
	if got := podEvents(known, terminations, podApp, watch.Modified, terminated, time.Now()); len(got) != 1 || got[0].Action != events.ActionDie {
		t.Fatalf("modified with a termination = %+v, want one die event", got)
	}
	if got := podEvents(known, terminations, podApp, watch.Deleted, terminated, time.Now()); got != nil {
		t.Fatalf("delete after the termination was already reported = %q, want no event", actions(got))
	}
}

// A run-once pod the kubelet never even started (rejected by admission, e.g.
// OutOfcpu) ends Failed with no ContainerStatuses at all: no termination is
// ever reported, so the normal die-on-termination path never fires. Without
// this, nothing dies until the pod is later deleted (by the next docker
// start replacing it), delivering a stale die during that next run.
func TestPodEventsReportsAnAdmissionRejectionDead(t *testing.T) {
	d := eventDeployment(0, map[string]string{types.AnnotationRestartPolicy: "no", types.AnnotationStartedAt: "2026-01-01T00:00:00Z"})
	known := map[string]*appsv1.Deployment{"web-k8s": d}
	terminations, podApp := map[string]int32{}, map[string]string{}
	rejected := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-k8s-abc", Labels: map[string]string{"app": "web-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed},
	}

	died := podEvents(known, terminations, podApp, watch.Modified, rejected, time.Now())
	if got := actions(died); !slices.Equal(got, []string{"container die"}) || died[0].Actor.Attributes["exitCode"] != "137" {
		t.Fatalf("admission-rejected pod = %q %v, want a single die with exit code 137", got, died)
	}

	// docker start later deletes the rejected pod to retry: no stale die.
	if got := podEvents(known, terminations, podApp, watch.Deleted, rejected, time.Now()); got != nil {
		t.Fatalf("delete after the rejection was already reported = %q, want no event", actions(got))
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

// An on-failure container that crashes is reported dead once per crash. The
// kubelet shows each crash as Terminated, then Waiting in back-off with the
// same restart count, then Running with the count raised: only the
// Terminated status is a new death.
func TestPodEventsReportEachCrashOnce(t *testing.T) {
	d := eventDeployment(0, map[string]string{types.AnnotationRestartPolicy: "on-failure", types.AnnotationStartedAt: "2026-01-01T00:00:00Z"})
	known := map[string]*appsv1.Deployment{"web-k8s": d}
	terminations, podApp := map[string]int32{}, map[string]string{}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	terminated := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}
	backOff := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	var died int
	for _, s := range []struct {
		restarts int32
		state    corev1.ContainerState
	}{
		{0, running}, {0, terminated}, {1, running},
		{1, terminated}, {1, backOff}, {2, running},
		{2, terminated}, {2, backOff}, {3, running},
	} {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-k8s-abc", Labels: map[string]string{"app": "web-k8s"}},
			Spec:       corev1.PodSpec{RestartPolicy: corev1.RestartPolicyOnFailure},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				RestartCount: s.restarts, State: s.state,
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
			}}},
		}
		died += len(podEvents(known, terminations, podApp, watch.Modified, p, time.Now()))
	}
	if died != 3 {
		t.Fatalf("die events = %d for 3 crashes, want 3", died)
	}
}

// docker stop of a container its Deployment restarts ends its pod without a
// restart: the kubelet shows its container terminated before the pod goes.
// As in Docker, that is a death, which compose's attached up waits for
// before it returns (D2K-058); a crash is still reported once.
func TestPodEventsReportAStopAsADeath(t *testing.T) {
	known := map[string]*appsv1.Deployment{"web-k8s": eventDeployment(1, nil)}
	terminations, podApp := map[string]int32{}, map[string]string{}
	pod := func(restarts int32, state corev1.ContainerState) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-k8s-abc", Labels: map[string]string{"app": "web-k8s"}},
			Spec:       corev1.PodSpec{RestartPolicy: corev1.RestartPolicyAlways},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				RestartCount: restarts, State: state,
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
			}}},
		}
	}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	var got []events.Message
	for _, p := range []*corev1.Pod{
		pod(0, running),
		pod(0, corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}), // a crash
		pod(1, running),
		pod(1, corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}), // docker stop
	} {
		got = append(got, podEvents(known, terminations, podApp, watch.Modified, p, time.Now())...)
	}
	if len(got) != 2 || got[1].Action != events.ActionDie || got[1].Actor.Attributes["exitCode"] != "0" {
		t.Fatalf("events = %q %v, want a die for the crash and one, with exit code 0, for the stop", actions(got), got)
	}
}

// A container its Deployment restarts runs again after each death, in the
// same pod (a crash) or a new one (the pod deleted by hand, drained or
// evicted). As in Docker, each restart is a start, which attached compose
// up waits for to follow the new run's output (D2K-070).
func TestEventsReportARestartAsAStart(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, _, err := a.CreateContainer(ctx, RunOptions{Name: "web", Image: "nginx", RestartPolicy: "always"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	pods := client.CoreV1().Pods(testNamespace)
	setPod := func(name string, restarts int32, state corev1.ContainerState) {
		t.Helper()
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: managedLabels("web")},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
				RestartCount: restarts, State: state,
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}},
			}}},
		}
		if _, err := pods.Get(ctx, name, metav1.GetOptions{}); err != nil {
			if _, err := pods.Create(ctx, p, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			return
		}
		if _, err := pods.UpdateStatus(ctx, p, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	crashed := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}}
	setPod("web-a", 0, running)

	_, live, unsubscribe := a.SubscribeEvents()
	defer unsubscribe()
	go a.RecordEvents(ctx)
	time.Sleep(200 * time.Millisecond)

	var got []string
	next := func() {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case m := <-live:
				if m.Actor.ID == id && (m.Action == events.ActionDie || m.Action == events.ActionStart) {
					got = append(got, string(m.Action))
					return
				}
			case <-deadline:
				t.Fatalf("events = %q, then nothing", got)
			}
		}
	}
	// A crash: the kubelet restarts the container in its pod.
	setPod("web-a", 0, crashed)
	next()
	setPod("web-a", 1, running)
	next()
	// The pod is deleted: its container ends and a new pod replaces it.
	setPod("web-a", 1, corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}})
	next()
	if err := pods.Delete(ctx, "web-a", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	setPod("web-b", 0, running)
	next()
	if want := []string{"die", "start", "die", "start"}; !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

// A death that docker start follows is not restarted by Kubernetes: the
// start the Deployment's change reports is the only one.
func TestRestartEventsLeaveDockerStartsAlone(t *testing.T) {
	stopped := time.Now()
	d := eventDeployment(1, map[string]string{types.AnnotationStartedAt: stopped.Add(time.Second).UTC().Format(time.RFC3339Nano)})
	s := &eventState{known: map[string]*appsv1.Deployment{"web-k8s": d}, running: map[string]string{}, died: map[string]death{}}
	if got := s.restartEvents("web-k8s", "web-k8s-a", false, true, stopped); got != nil {
		t.Fatalf("death = %q, want no event", actions(got))
	}
	if got := s.restartEvents("web-k8s", "web-k8s-b", true, false, stopped.Add(2*time.Second)); got != nil {
		t.Fatalf("pod of docker start runs = %q, want no event: docker start reported it", actions(got))
	}
}

// A container stopped while d2k's watch restarts may have its pod gone by
// the relist, its termination never seen. Docker reports die, then stop;
// attached compose up drops a container only on its die (D2K-058).
func TestRelistReportsAStoppedContainerWhosePodWentDead(t *testing.T) {
	s := &eventState{
		known: map[string]*appsv1.Deployment{}, terminations: map[string]int32{}, podApp: map[string]string{},
		running: map[string]string{}, died: map[string]death{}, volumes: map[string]bool{},
	}
	running := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-k8s-abc", Labels: map[string]string{"app": "web-k8s"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
	always := map[string]string{types.AnnotationRestartPolicy: "always"}
	s.relist([]appsv1.Deployment{*eventDeployment(1, always)}, []corev1.Pod{running}, nil, time.Now())

	got := actions(s.relist([]appsv1.Deployment{*eventDeployment(0, always)}, nil, nil, time.Now()))
	if want := []string{"container die", "container stop"}; !slices.Equal(got, want) {
		t.Fatalf("relist after a stop = %q, want %q", got, want)
	}

	// Kubernetes replaced the pod while nothing watched: the container died
	// and runs again, as a restart policy restart does in Docker.
	s.relist([]appsv1.Deployment{*eventDeployment(1, always)}, []corev1.Pod{running}, nil, time.Now())
	replacement := *running.DeepCopy()
	replacement.Name = "web-k8s-def"
	got = actions(s.relist([]appsv1.Deployment{*eventDeployment(1, always)}, []corev1.Pod{replacement}, nil, time.Now()))
	if want := []string{"container die", "container start"}; !slices.Equal(got, want) {
		t.Fatalf("relist after a pod replacement = %q, want %q", got, want)
	}
}

// A restart cut short by d2k stopping, between its stop and its start, is
// finished when d2k starts: as the Docker daemon does when it starts, an
// always or unless-stopped container runs again. A container that runs once
// stays stopped, as Docker leaves it (D2K-083).
func TestEventsFinishRestartsCutShortByD2kStopping(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", RestartPolicy: "always"})
	mustCreate(t, a, RunOptions{Name: "job", Image: "busybox", RestartPolicy: "no"})
	for _, name := range []string{"web", "job"} {
		if err := a.StartContainer(ctx, name); err != nil {
			t.Fatal(err)
		}
		if err := a.annotate(ctx, name, map[string]*string{"d2k.portainer.io/restarting": ptr("2026-01-01T00:00:00Z")}); err != nil {
			t.Fatal(err)
		}
		if err := a.StopContainer(ctx, name); err != nil {
			t.Fatal(err)
		}
	}

	go a.RecordEvents(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		web, err := client.AppsV1().Deployments(testNamespace).Get(ctx, "web", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		job, err := client.AppsV1().Deployments(testNamespace).Get(ctx, "job", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, webMarked := web.Annotations["d2k.portainer.io/restarting"]
		_, jobMarked := job.Annotations["d2k.portainer.io/restarting"]
		if *web.Spec.Replicas == 1 && !webMarked && !jobMarked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("web replicas = %d, restarting web %v, job %v; want 1, the restarts finished", *web.Spec.Replicas, webMarked, jobMarked)
		}
		time.Sleep(50 * time.Millisecond)
	}
	job, err := a.InspectContainer(ctx, "job")
	if err != nil {
		t.Fatal(err)
	}
	if job.State.Status != "exited" {
		t.Errorf("job status = %q, want exited", job.State.Status)
	}
}

// A restart d2k finishes when it starts is a start like any other: one whose
// pod Kubernetes refuses to create, as a full pod quota does, leaves the
// container stopped, not at 1 replica to start by itself once the quota
// frees (D2K-084), and no longer restarting.
func TestEventsFinishARestartTheQuotaRefuses(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", RestartPolicy: "always"})
	if err := a.annotate(ctx, "web", map[string]*string{"d2k.portainer.io/restarting": ptr("2026-01-01T00:00:00Z")}); err != nil {
		t.Fatal(err)
	}
	d, err := client.AppsV1().Deployments(testNamespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d.Status.Conditions = []appsv1.DeploymentCondition{
		{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate",
			Message: `pods "web-6d4b9-x2k8p" is forbidden: exceeded quota: pods, requested: pods=1, used: pods=4, limited: pods=4`},
	}
	if _, err := client.AppsV1().Deployments(testNamespace).UpdateStatus(ctx, d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	go a.RecordEvents(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		d, err := client.AppsV1().Deployments(testNamespace).Get(ctx, "web", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, marked := d.Annotations["d2k.portainer.io/restarting"]; !marked {
			if r := replicasOf(d); r != 0 {
				t.Fatalf("replicas = %d after a finished restart the quota refused, want 0", r)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still restarting at %d replicas, want the restart finished, stopped", replicasOf(d))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A restart another d2k finishes meanwhile, as the d2k a rollout replaces
// does while the new one starts, is not finished again: a docker stop that
// follows it stays stopped.
func TestEventsLeaveARestartFinishedMeanwhile(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", RestartPolicy: "always"})
	if err := a.StartContainer(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if err := a.annotate(ctx, "web", map[string]*string{"d2k.portainer.io/restarting": ptr("2026-01-01T00:00:00Z")}); err != nil {
		t.Fatal(err)
	}
	d, err := client.AppsV1().Deployments(testNamespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The run the other d2k's restart started.
	running := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: testNamespace, Labels: d.Spec.Template.Labels}}
	if err := client.Tracker().Add(running); err != nil {
		t.Fatal(err)
	}

	go a.RecordEvents(ctx)
	time.Sleep(time.Second)
	// The other d2k clears the mark; then docker stop ends the run.
	if err := a.annotate(ctx, "web", map[string]*string{"d2k.portainer.io/restarting": nil}); err != nil {
		t.Fatal(err)
	}
	if err := a.scaleDeployment(ctx, "web", 0); err != nil {
		t.Fatal(err)
	}
	if err := client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), testNamespace, "web-abc"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if d, err := client.AppsV1().Deployments(testNamespace).Get(ctx, "web", metav1.GetOptions{}); err != nil || replicasOf(d) != 0 {
		t.Fatalf("replicas = %d, %v after docker stop, want 0: the restart was already finished", replicasOf(d), err)
	}
}

// A restart d2k finishes starts the container again only once the run its
// stop ended is gone, as the restart itself does: the two never run side by
// side on the container's volumes (D2K-083).
func TestEventsFinishARestartOnceItsStoppedPodIsGone(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mustCreate(t, a, RunOptions{Name: "db", Image: "postgres", RestartPolicy: "always"})
	if err := a.annotate(ctx, "db", map[string]*string{"d2k.portainer.io/restarting": ptr("2026-01-01T00:00:00Z")}); err != nil {
		t.Fatal(err)
	}
	d, err := client.AppsV1().Deployments(testNamespace).Get(ctx, "db", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The pod the restart's stop ended, still in its stop grace period.
	stopping := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "db-abc", Namespace: testNamespace, Labels: d.Spec.Template.Labels,
		DeletionTimestamp: &metav1.Time{Time: time.Now()}, Finalizers: []string{"test/hold"},
	}}
	if err := client.Tracker().Add(stopping); err != nil {
		t.Fatal(err)
	}

	go a.RecordEvents(ctx)
	time.Sleep(time.Second)
	if d, err := client.AppsV1().Deployments(testNamespace).Get(ctx, "db", metav1.GetOptions{}); err != nil || *d.Spec.Replicas != 0 {
		t.Fatalf("replicas = %d, %v while the stopped pod remains, want 0", *d.Spec.Replicas, err)
	}
	if err := client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), testNamespace, "db-abc"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		d, err := client.AppsV1().Deployments(testNamespace).Get(ctx, "db", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if *d.Spec.Replicas == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the restart was not finished once the stopped pod was gone")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
