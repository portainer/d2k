package containers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/events"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/portainer/d2k/internal/adapter"
)

// An attached docker compose up inspects a container when it dies, and
// keeps following it if it is restarting. As in Docker, a container that
// docker restart (or compose restart) stops is not reported exited.
func TestRestartingContainerIsNotReportedExited(t *testing.T) {
	client := newFakeCluster()
	a := adapter.NewWithClient(client, "d2k")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go a.RecordEvents(ctx)
	h := NewHandler(a)

	rec := httptest.NewRecorder()
	h.Create(rec, httptest.NewRequest("POST", "/containers/create?name=web", strings.NewReader(`{"Image":"nginx"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	time.Sleep(200 * time.Millisecond) // the watch lists
	if err := a.StartContainer(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	pods, err := client.CoreV1().Pods("d2k").List(ctx, metav1.ListOptions{LabelSelector: "app=web"})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods = %v, %v", pods, err)
	}
	p := pods.Items[0]
	p.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
	}}}
	if _, err := client.CoreV1().Pods("d2k").UpdateStatus(ctx, &p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	kubeletStops(ctx, client)

	_, live, stop := a.SubscribeEvents()
	defer stop()
	restarted := make(chan *httptest.ResponseRecorder)
	go func() {
		rec := httptest.NewRecorder()
		h.Restart(rec, httptest.NewRequest("POST", "/containers/web/restart", nil).WithContext(ctx))
		restarted <- rec
	}()
	for {
		select {
		case m := <-live:
			if m.Action != events.ActionDie {
				continue
			}
			ctr, err := a.InspectContainer(ctx, "web")
			if err != nil {
				t.Fatal(err)
			}
			if !ctr.State.Running && !ctr.State.Restarting {
				t.Errorf("state on the restart's die = %s, want running or restarting", ctr.State.Status)
			}
			if rec := <-restarted; rec.Code != http.StatusNoContent {
				t.Errorf("restart = %d %s, want 204", rec.Code, rec.Body)
			}
			return
		case <-ctx.Done():
			t.Fatal("no die event for the restart")
		}
	}
}

// As in Docker, docker restart (or compose restart) returns once the
// container runs again: docker ps and compose ps, pause or port right after
// it see the container running, not missing.
func TestRestartedContainerIsListedWhenRestartReturns(t *testing.T) {
	client := newFakeCluster()
	a := adapter.NewWithClient(client, "d2k")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	kubeletRuns(ctx, client)
	kubeletStops(ctx, client)
	go a.RecordEvents(ctx)
	h := NewHandler(a)

	rec := httptest.NewRecorder()
	h.Create(rec, httptest.NewRequest("POST", "/containers/create?name=web", strings.NewReader(`{"Image":"nginx"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	if err := a.StartContainer(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	for ctr, err := a.InspectContainer(ctx, "web"); err != nil || !ctr.State.Running; ctr, err = a.InspectContainer(ctx, "web") {
		if ctx.Err() != nil {
			t.Fatal("the container did not run")
		}
		time.Sleep(50 * time.Millisecond)
	}

	rec = httptest.NewRecorder()
	h.Restart(rec, httptest.NewRequest("POST", "/containers/web/restart", nil).WithContext(ctx))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("restart = %d %s, want 204", rec.Code, rec.Body)
	}
	listed, err := a.ListContainers(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].State != "running" {
		t.Errorf("docker ps right after restart = %+v, want web running", listed)
	}
}

// kubeletRuns makes the cluster run the pods it creates, as the kubelet
// does, a moment after they are created.
func kubeletRuns(ctx context.Context, client *fake.Clientset) {
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		name := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Name
		go func() {
			time.Sleep(300 * time.Millisecond)
			p, err := client.CoreV1().Pods("d2k").Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return
			}
			p.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
			}}}
			_, _ = client.CoreV1().Pods("d2k").UpdateStatus(ctx, p, metav1.UpdateOptions{})
		}()
		return false, nil, nil
	})
}
