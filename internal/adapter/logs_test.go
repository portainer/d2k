package adapter

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/portainer/d2k/internal/types"
)

// Unlike Docker, where a container is either not started or running, a
// Kubernetes pod spends a few seconds ContainerCreating. The kubelet refuses
// to stream a container's logs during that time, and d2k must wait it out
// rather than pass the kubelet's error straight to the client.
func TestGetContainerLogsWaitsForContainerCreating(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", RestartPolicy: "no"})
	if err := a.StartContainer(ctx, "web"); err != nil {
		t.Fatal(err)
	}

	pods, err := client.CoreV1().Pods(testNamespace).List(ctx, podsOf("web"))
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("expected one pod for web, got %v, %v", pods, err)
	}
	pod := pods.Items[0]
	pod.Status = corev1.PodStatus{
		Phase:             corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}},
	}
	if _, err := client.CoreV1().Pods(testNamespace).UpdateStatus(ctx, &pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := a.GetContainerLogs(ctx, "web", LogOptions{})
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("GetContainerLogs returned (%v) while the pod is still ContainerCreating", err)
	case <-time.After(300 * time.Millisecond):
	}

	pod.Status = corev1.PodStatus{
		Phase:             corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}},
	}
	if _, err := client.CoreV1().Pods(testNamespace).UpdateStatus(ctx, &pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetContainerLogs did not return once the pod started running")
	}
}

// Right after the pod is created, the kubelet has not reported any container
// status yet: ContainerStatuses is empty, not a Waiting state. d2k must wait
// this out too, the same as an explicit ContainerCreating/PodInitializing
// Waiting reason, instead of treating "no status yet" as already started.
func TestGetContainerLogsWaitsWhenNoContainerStatusYet(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", RestartPolicy: "no"})
	if err := a.StartContainer(ctx, "web"); err != nil {
		t.Fatal(err)
	}

	pods, err := client.CoreV1().Pods(testNamespace).List(ctx, podsOf("web"))
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("expected one pod for web, got %v, %v", pods, err)
	}
	pod := pods.Items[0]
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending}
	if _, err := client.CoreV1().Pods(testNamespace).UpdateStatus(ctx, &pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := a.GetContainerLogs(ctx, "web", LogOptions{})
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("GetContainerLogs returned (%v) before the kubelet reported any container status", err)
	case <-time.After(300 * time.Millisecond):
	}

	pod.Status = corev1.PodStatus{
		Phase:             corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}},
	}
	if _, err := client.CoreV1().Pods(testNamespace).UpdateStatus(ctx, &pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetContainerLogs did not return once the pod started running")
	}
}

// A pod the scheduler cannot place has no container status either, but it
// is not about to start: docker logs answers at once, as Docker does for a
// created container.
func TestGetContainerLogsDoesNotWaitForAnUnschedulablePod(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	mustCreate(t, a, RunOptions{Name: "big", Image: "nginx", RestartPolicy: "no"})
	if err := a.StartContainer(ctx, "big"); err != nil {
		t.Fatal(err)
	}
	pods, err := client.CoreV1().Pods(testNamespace).List(ctx, podsOf("big"))
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("expected one pod for big, got %v, %v", pods, err)
	}
	pod := pods.Items[0]
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
	}}}
	if _, err := client.CoreV1().Pods(testNamespace).UpdateStatus(ctx, &pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		_, _, _ = a.GetContainerLogs(ctx, "big", LogOptions{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("GetContainerLogs waited for a pod the scheduler cannot place")
	}
}

// docker logs of a stopped container applies --since, --tail and
// --timestamps to the kept log as to a live one.
func TestKeptLog(t *testing.T) {
	log := "2026-10-05T12:00:00.000000001Z first\n2026-10-05T12:00:01.000000001Z second\n2026-10-05T12:00:02.000000001Z third\n"
	tail := int64(2)
	since := metav1.NewTime(time.Date(2026, 10, 5, 12, 0, 1, 0, time.UTC))
	for _, tc := range []struct {
		name string
		opts corev1.PodLogOptions
		want string
	}{
		{"all", corev1.PodLogOptions{}, "first\nsecond\nthird\n"},
		{"tail", corev1.PodLogOptions{TailLines: &tail}, "second\nthird\n"},
		{"since", corev1.PodLogOptions{SinceTime: &since}, "second\nthird\n"},
		{"timestamps", corev1.PodLogOptions{TailLines: &tail, Timestamps: true}, "2026-10-05T12:00:01.000000001Z second\n2026-10-05T12:00:02.000000001Z third\n"},
	} {
		if got := keptLog(log, &tc.opts); got != tc.want {
			t.Errorf("%s: kept log = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// docker logs -f follows until the container stops, as in Docker. The
// kubelet's log stream can end before that while the container still runs
// (the API server or the kubelet restarting); following carries on then,
// and ends once the container has stopped. (Docker also ends a follow when
// the container exits and its restart policy restarts it.)
func TestFollowLogsUntilTheContainerStops(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", RestartPolicy: "always"})
	if err := a.StartContainer(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: testNamespace,
		Labels: map[string]string{"app": "web", types.LabelManagedBy: types.LabelManagedByValue}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}}}}
	if _, err := client.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// The fake clientset's log stream ends at once, as a dropped one does.
	logs, _, err := a.GetContainerLogs(ctx, "web", LogOptions{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, logs)
		close(ended)
	}()
	select {
	case <-ended:
		t.Fatal("following ended while the container still runs")
	case <-time.After(1500 * time.Millisecond):
	}

	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, FinishedAt: metav1.Now()}}
	if _, err := client.CoreV1().Pods(testNamespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("following did not end once the container stopped")
	}
	_ = logs.Close()
}

// While a stopping container's pod lasts, d2k reads the end of its log
// again and again. A later read can hold only the last few lines (seen on
// kind: nginx's shutdown lines alone), so it adds to what was kept rather
// than replacing it.
func TestMergeLogTail(t *testing.T) {
	line := func(sec int, text string) string {
		return time.Date(2026, 10, 5, 12, 0, sec, 1, time.UTC).Format(time.RFC3339Nano) + " " + text + "\n"
	}
	kept := line(0, "start") + line(1, "ready") + line(2, "request")
	for _, tc := range []struct {
		name, kept, tail, want string
	}{
		{"nothing kept yet", "", kept, kept},
		{"later read with only the newest lines", kept, line(3, "stopping"), kept + line(3, "stopping")},
		{"later read overlapping", kept, line(2, "request") + line(3, "stopping"), kept + line(3, "stopping")},
		{"later read with nothing new", kept, line(1, "ready") + line(2, "request"), kept},
		{"empty read", kept, "", kept},
	} {
		if got := mergeLogTail(tc.kept, tc.tail); got != tc.want {
			t.Errorf("%s: merged = %q, want %q", tc.name, got, tc.want)
		}
	}

	var many strings.Builder
	for i := range keptLogLines + 10 {
		many.WriteString(line(i%60, fmt.Sprintf("line %d", i)))
	}
	if got := strings.Count(mergeLogTail("", many.String()), "\n"); got > keptLogLines {
		t.Errorf("merged %d lines, want at most %d", got, keptLogLines)
	}
}
