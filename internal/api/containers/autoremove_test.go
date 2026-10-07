package containers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/internal/errdefs"
)

// `docker run --rm` waits for the container to be removed, and exits with
// the code its run ended with. As in Docker, the container is removed when
// its run ends.
func TestAutoRemove(t *testing.T) {
	client := newFakeCluster()
	a := adapter.NewWithClient(client, "d2k")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go a.RecordEvents(ctx)
	h := NewHandler(a)

	rec := httptest.NewRecorder()
	h.Create(rec, httptest.NewRequest("POST", "/containers/create?name=job", strings.NewReader(`{"Image":"busybox","HostConfig":{"AutoRemove":true}}`)))
	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), "AutoRemove") {
		t.Fatalf("create = %d %s, want 201 without an AutoRemove warning", rec.Code, rec.Body)
	}
	inspected, err := a.InspectContainer(ctx, "job")
	if err != nil {
		t.Fatal(err)
	}
	if !inspected.HostConfig.AutoRemove {
		t.Errorf("inspect HostConfig.AutoRemove = false, want true")
	}

	waited := make(chan *httptest.ResponseRecorder)
	go func() {
		rec := httptest.NewRecorder()
		h.Wait(rec, httptest.NewRequest("POST", "/containers/job/wait?condition=removed", nil).WithContext(ctx))
		waited <- rec
	}()
	time.Sleep(200 * time.Millisecond) // the watch and the wait subscribe
	if err := a.StartContainer(ctx, "job"); err != nil {
		t.Fatal(err)
	}
	pods, err := client.CoreV1().Pods("d2k").List(ctx, metav1.ListOptions{LabelSelector: "app=job"})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods = %v, %v", pods, err)
	}
	p := pods.Items[0]
	p.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3, FinishedAt: metav1.Now()}},
	}}}
	if _, err := client.CoreV1().Pods("d2k").UpdateStatus(ctx, &p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	select {
	case rec := <-waited:
		var resp container.WaitResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.StatusCode != 3 {
			t.Errorf("wait = %d %s, want exit code 3", rec.Code, rec.Body)
		}
	case <-ctx.Done():
		t.Fatal("wait with condition removed did not return: the container was not removed")
	}
	if _, err := a.InspectContainer(ctx, "job"); !errdefs.IsNotFound(err) {
		t.Errorf("inspect after the run = %v, want not found", err)
	}
}

// runRemovable creates and starts a --rm container named name, with the
// event watch running, and returns its pod.
func runRemovable(t *testing.T, ctx context.Context, client *fake.Clientset, a *adapter.KubernetesDockerAdapter, h *Handler, name string) corev1.Pod {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Create(rec, httptest.NewRequest("POST", "/containers/create?name="+name, strings.NewReader(`{"Image":"busybox","HostConfig":{"AutoRemove":true}}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	time.Sleep(200 * time.Millisecond) // the watch lists
	if err := a.StartContainer(ctx, name); err != nil {
		t.Fatal(err)
	}
	pods, err := client.CoreV1().Pods("d2k").List(ctx, metav1.ListOptions{LabelSelector: "app=" + name})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods = %v, %v", pods, err)
	}
	return pods.Items[0]
}

// kubeletStops makes the cluster stop pods as the kubelet does: deleting a
// pod terminates its container first, and the pod goes once it has had
// time to be seen terminated.
func kubeletStops(ctx context.Context, client *fake.Clientset) {
	podsGVR := corev1.SchemeGroupVersion.WithResource("pods")
	client.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj, err := client.Tracker().Get(podsGVR, "d2k", action.(k8stesting.DeleteAction).GetName())
		if err != nil {
			return false, nil, nil
		}
		p := obj.(*corev1.Pod).DeepCopy()
		if p.Status.Phase == corev1.PodFailed {
			return false, nil, nil
		}
		p.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 143, FinishedAt: metav1.Now()}},
		}}}
		if err := client.Tracker().Update(podsGVR, p, "d2k"); err != nil {
			return true, nil, err
		}
		go func() {
			time.Sleep(time.Second)
			_ = client.CoreV1().Pods("d2k").Delete(ctx, p.Name, metav1.DeleteOptions{})
		}()
		return true, nil, nil
	})
}

// As in Docker, stopping a --rm container succeeds, and then removes it.
func TestAutoRemoveStop(t *testing.T) {
	client := newFakeCluster()
	a := adapter.NewWithClient(client, "d2k")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go a.RecordEvents(ctx)
	h := NewHandler(a)
	runRemovable(t, ctx, client, a, h, "job")
	kubeletStops(ctx, client)

	rec := httptest.NewRecorder()
	h.Stop(rec, httptest.NewRequest("POST", "/containers/job/stop", nil).WithContext(ctx))
	if rec.Code != http.StatusNoContent {
		t.Errorf("stop = %d %s, want 204", rec.Code, rec.Body)
	}
	if _, err := a.InspectContainer(ctx, "job"); !errdefs.IsNotFound(err) {
		t.Errorf("inspect after stop = %v, want not found", err)
	}
}

// As in Docker, restarting a --rm container keeps it: only the end of a run
// that is not restarted removes it.
func TestAutoRemoveRestart(t *testing.T) {
	client := newFakeCluster()
	a := adapter.NewWithClient(client, "d2k")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	kubeletRuns(ctx, client)
	go a.RecordEvents(ctx)
	h := NewHandler(a)
	runRemovable(t, ctx, client, a, h, "job")
	kubeletStops(ctx, client)

	rec := httptest.NewRecorder()
	h.Restart(rec, httptest.NewRequest("POST", "/containers/job/restart", nil).WithContext(ctx))
	if rec.Code != http.StatusNoContent {
		t.Errorf("restart = %d %s, want 204", rec.Code, rec.Body)
	}
	time.Sleep(500 * time.Millisecond) // a removal would have happened
	ctr, err := a.InspectContainer(ctx, "job")
	if err != nil {
		t.Fatalf("inspect after restart = %v, want the container", err)
	}
	if !adapter.IsStarting(ctr.State) && !ctr.State.Running {
		t.Errorf("state after restart = %+v, want started", ctr.State)
	}
}

// As in Docker, a container that restarts cannot be removed when it exits.
func TestAutoRemoveRefusesRestartPolicy(t *testing.T) {
	h := NewHandler(adapter.NewWithClient(newFakeCluster(), "d2k"))
	rec := httptest.NewRecorder()
	h.Create(rec, httptest.NewRequest("POST", "/containers/create?name=web", strings.NewReader(`{"Image":"nginx","HostConfig":{"AutoRemove":true,"RestartPolicy":{"Name":"always"}}}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "can't create 'AutoRemove' container with restart policy") {
		t.Fatalf("create = %d %s, want 400", rec.Code, rec.Body)
	}
}

// newFakeCluster returns a fake cluster with d2k's namespace, which assigns
// UIDs on create as the API server does.
func newFakeCluster() *fake.Clientset {
	client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "d2k"}})
	client.PrependReactor("create", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if obj, err := meta.Accessor(action.(k8stesting.CreateAction).GetObject()); err == nil && obj.GetUID() == "" {
			obj.SetUID(uuid.NewUUID())
		}
		return false, nil, nil
	})
	return client
}

// docker run -d --rm, then docker wait: a short run that ends, and is
// removed, while the wait still sees the container starting answers with
// the run's exit code, as in Docker, rather than "no such container".
func TestWaitOnAStartingRunRemovedWhenItEnds(t *testing.T) {
	client := newFakeCluster()
	a := adapter.NewWithClient(client, "d2k")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go a.RecordEvents(ctx)
	h := NewHandler(a)
	p := runRemovable(t, ctx, client, a, h, "job")

	waited := make(chan *httptest.ResponseRecorder)
	go func() {
		rec := httptest.NewRecorder()
		h.Wait(rec, httptest.NewRequest("POST", "/containers/job/wait", nil).WithContext(ctx))
		waited <- rec
	}()
	time.Sleep(100 * time.Millisecond) // the wait sees the container starting
	p.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3, FinishedAt: metav1.Now()}},
	}}}
	if _, err := client.CoreV1().Pods("d2k").UpdateStatus(ctx, &p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	select {
	case rec := <-waited:
		var resp container.WaitResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); rec.Code != http.StatusOK || err != nil || resp.StatusCode != 3 {
			t.Errorf("wait = %d %s, want 200 with exit code 3", rec.Code, rec.Body)
		}
	case <-ctx.Done():
		t.Fatal("wait did not return")
	}
}
