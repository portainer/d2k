package containers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/portainer/d2k/internal/adapter"
)

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
