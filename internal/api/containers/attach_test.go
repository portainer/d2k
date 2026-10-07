package containers

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/moby/moby/api/types/events"
	"github.com/portainer/d2k/internal/adapter"
)

// A pod force-deleted or lost out from under an attach never gets the
// kubelet to end the SPDY stream: podEvents still reports the container's
// die (D2K-035), but nothing made the attach notice. stopOnDeath is the
// backstop: watching the same event stream Wait uses, it cancels the
// attach's context as soon as the container's die event arrives, so the
// handler's stream ends instead of hanging after inspect/wait already
// report the container dead (D2K-038).
func TestStopOnDeathCancelsOnDie(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	live := make(chan events.Message, 2)
	live <- events.Message{Actor: events.Actor{ID: "other"}, Action: events.ActionDie}
	live <- events.Message{Actor: events.Actor{ID: "ctr1"}, Action: events.ActionDie}

	done := make(chan struct{})
	go func() {
		stopOnDeath(ctx, stop, "ctr1", live)
		close(done)
	}()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stopOnDeath did not cancel on a die event for this container")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stopOnDeath did not return after cancelling")
	}
}

// Other events, and another container's die, must not end the attach.
func TestStopOnDeathIgnoresOtherEvents(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	live := make(chan events.Message, 1)
	done := make(chan struct{})
	go func() {
		stopOnDeath(ctx, stop, "ctr1", live)
		close(done)
	}()

	live <- events.Message{Actor: events.Actor{ID: "other"}, Action: events.ActionDie}
	select {
	case <-ctx.Done():
		t.Fatal("stopOnDeath cancelled on another container's die event")
	case <-time.After(200 * time.Millisecond):
	}

	stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stopOnDeath did not return once ctx ended")
	}
}

// `docker run` attaches before it starts the container. A container that
// runs once may exit before it is ever seen running: the attach must still
// end, with its output, instead of waiting for it to run.
func TestWaitUntilStartedSeesARunThatEnded(t *testing.T) {
	client := fake.NewClientset()
	a := adapter.NewWithClient(client, "d2k")
	ctx := context.Background()
	if _, _, err := a.CreateContainer(ctx, adapter.RunOptions{Name: "job", Image: "busybox", RestartPolicy: "no"}); err != nil {
		t.Fatal(err)
	}
	before, err := a.InspectContainer(ctx, "job")
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		_ = a.StartContainer(ctx, "job")
		pods, _ := client.CoreV1().Pods("d2k").List(ctx, metav1.ListOptions{LabelSelector: "app=job"})
		for _, p := range pods.Items {
			p.Status = corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, FinishedAt: metav1.Now()}},
			}}}
			_, _ = client.CoreV1().Pods("d2k").UpdateStatus(ctx, &p, metav1.UpdateOptions{})
		}
	}()

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	running, ok := waitUntilStarted(waitCtx, a, before.ID, before.State.StartedAt)
	if !ok || running {
		t.Fatalf("waitUntilStarted = running %v, ok %v; want a run that ended", running, ok)
	}
}

// A resize applies to the container's TTY, whoever is attached: when one of
// two attaches ends, the other still gets the next size.
func TestResizeReachesTheRemainingAttach(t *testing.T) {
	a := adapter.NewWithClient(fake.NewClientset(), "d2k")
	ctx := context.Background()
	if _, _, err := a.CreateContainer(ctx, adapter.RunOptions{Name: "sh", Image: "busybox", Tty: true, Stdin: true}); err != nil {
		t.Fatal(err)
	}
	ctr, err := a.InspectContainer(ctx, "sh")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(a)
	first := h.attachSession(ctr.ID)
	defer h.endAttachSession(ctr.ID, first)
	second := h.attachSession(ctr.ID)
	h.endAttachSession(ctr.ID, second)

	rec := httptest.NewRecorder()
	h.Resize(rec, httptest.NewRequest("POST", "/containers/sh/resize?h=33&w=77", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("resize = %d, want 200", rec.Code)
	}
	got := make(chan *remotecommand.TerminalSize, 1)
	go func() { got <- first.Next() }()
	select {
	case size := <-got:
		if size == nil || size.Height != 33 || size.Width != 77 {
			t.Fatalf("first attach got %v, want 33x77", size)
		}
	case <-time.After(time.Second):
		t.Fatal("the first attach never got the resize")
	}
}

// `docker run busybox echo hi` attaches before it starts the container,
// which exits at once. Its die event arrives while the attach still waits
// for it to run, or still copies its log: the attach must send that output
// rather than end on the die with nothing written.
func TestAttachSendsTheOutputOfARunThatEnded(t *testing.T) {
	client := newFakeCluster()
	a := adapter.NewWithClient(client, "d2k")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go a.RecordEvents(ctx)
	h := NewHandler(a)

	rec := httptest.NewRecorder()
	h.Create(rec, httptest.NewRequest("POST", "/containers/create?name=job", strings.NewReader(`{"Image":"busybox","Cmd":["echo","hi"]}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	time.Sleep(200 * time.Millisecond) // the watch lists

	srv := httptest.NewServer(http.HandlerFunc(h.Attach))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, "POST /containers/job/attach?stream=1&stdout=1&stderr=1 HTTP/1.1\r\nHost: d2k\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(conn)
		got <- string(b)
	}()
	time.Sleep(200 * time.Millisecond) // the attach subscribes and waits for the run

	if err := a.StartContainer(ctx, "job"); err != nil {
		t.Fatal(err)
	}
	pods, err := client.CoreV1().Pods("d2k").List(ctx, metav1.ListOptions{LabelSelector: "app=job"})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods = %v, %v", pods, err)
	}
	p := pods.Items[0]
	p.Status = corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, FinishedAt: metav1.Now()}},
	}}}
	if _, err := client.CoreV1().Pods("d2k").UpdateStatus(ctx, &p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	// The fake cluster's log of any pod is "fake logs".
	if out := <-got; !strings.Contains(out, "fake logs") {
		t.Fatalf("attach sent %q, want the container's log", out)
	}
}
