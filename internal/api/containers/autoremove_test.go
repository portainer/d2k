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
