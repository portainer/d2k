package router

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Docker's start returns once the container runs, and compose runs
// post_start hooks right after it. Exec on a container whose pod is still
// starting waits for it; exec on a stopped container fails at once.
func TestExecWaitsForAStartingContainer(t *testing.T) {
	h, client := newTestRouter()
	if rec := do(t, h, "POST", "/containers/create?name=web", `{"Image":"nginx"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/containers/web/exec", `{"Cmd":["true"]}`); rec.Code != http.StatusConflict {
		t.Fatalf("exec on a container never started = %d, want 409", rec.Code)
	}
	if rec := do(t, h, "POST", "/containers/web/start", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}

	go func() {
		time.Sleep(300 * time.Millisecond)
		_, _ = client.CoreV1().Pods("d2k").Create(context.Background(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "d2k", Labels: map[string]string{"app": "web"}, CreationTimestamp: metav1.Now()},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
			}}},
		}, metav1.CreateOptions{})
	}()
	if rec := do(t, h, "POST", "/containers/web/exec", `{"Cmd":["true"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("exec on a starting container = %d %s, want 201 once it runs", rec.Code, rec.Body)
	}
}

// Docker's start returns once the container runs, so `docker start` then
// `docker wait` waits for that run to end. d2k's start returns while the pod
// is starting: wait must not answer before the run.
func TestWaitAfterStartWaitsForTheRun(t *testing.T) {
	h, client := newTestRouter()
	if rec := do(t, h, "POST", "/containers/create?name=job", `{"Image":"busybox"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/containers/job/start", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		pods, _ := client.CoreV1().Pods("d2k").List(context.Background(), metav1.ListOptions{LabelSelector: "app=job"})
		for _, p := range pods.Items {
			p.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3, StartedAt: metav1.Now(), FinishedAt: metav1.Now()}},
			}}}
			_, _ = client.CoreV1().Pods("d2k").UpdateStatus(context.Background(), &p, metav1.UpdateOptions{})
		}
	}()
	if rec := do(t, h, "POST", "/containers/job/wait", ""); !strings.Contains(rec.Body.String(), `"StatusCode":3`) {
		t.Fatalf("wait = %d %s, want the run's exit code 3", rec.Code, rec.Body)
	}
}
