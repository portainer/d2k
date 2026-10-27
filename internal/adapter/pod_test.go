package adapter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

// startPod returns the error from listing the container's pods, instead of
// treating it as if there were no previous pod to replace.
func TestStartPodListPodsError(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	id, _, err := a.CreateContainer(ctx, RunOptions{Name: "job", Image: "busybox", RestartPolicy: "no"})
	if err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("list failed")
	})
	if err := a.StartContainer(ctx, id); err == nil || !strings.Contains(err.Error(), "unable to list the pods") {
		t.Errorf("StartContainer error = %v, want an \"unable to list the pods\" error", err)
	}
}

// startPod reports a failure to delete a previous, already-ended pod,
// instead of silently leaving it behind.
func TestStartPodDeleteOldPodError(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	id, _, err := a.CreateContainer(ctx, RunOptions{Name: "job", Image: "busybox", RestartPolicy: "no"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	p := podOf(t, a, "job")
	setStatus(t, a, p, corev1.PodSucceeded, corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}})

	client.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, p.Name, errors.New("conflict"))
	})
	if err := a.StartContainer(ctx, id); err == nil || !strings.Contains(err.Error(), "unable to delete the previous pod") {
		t.Errorf("StartContainer error = %v, want an \"unable to delete the previous pod\" error", err)
	}
}

// stopPod returns the error from listing the container's pods.
func TestStopPodListPodsError(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	id, _, err := a.CreateContainer(ctx, RunOptions{Name: "job", Image: "busybox", RestartPolicy: "no"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("list failed")
	})
	if err := a.StopContainer(ctx, id, nil); err == nil || !strings.Contains(err.Error(), "unable to list the pods") {
		t.Errorf("StopContainer error = %v, want an \"unable to list the pods\" error", err)
	}
}

// stopPod reports a failure to delete the running pod, instead of reporting
// the container stopped when it never was.
func TestStopPodDeleteError(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	id, _, err := a.CreateContainer(ctx, RunOptions{Name: "job", Image: "busybox", RestartPolicy: "no"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	p := podOf(t, a, "job")
	setStatus(t, a, p, corev1.PodRunning, corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}})

	client.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, p.Name, errors.New("conflict"))
	})
	if err := a.StopContainer(ctx, id, nil); err == nil || !strings.Contains(err.Error(), "unable to stop the pod") {
		t.Errorf("StopContainer error = %v, want an \"unable to stop the pod\" error", err)
	}
}

// waitUntilRunStarted gives up once its timeout passes, rather than blocking
// restart forever on a pod that never leaves "starting".
func TestWaitUntilRunStartedTimesOutWhileStarting(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()
	id, _, err := a.CreateContainer(ctx, RunOptions{Name: "web", Image: "nginx"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	// The pod never reports a status, so the container is "starting" forever.
	done := make(chan struct{})
	start := time.Now()
	go func() { a.waitUntilRunStarted(ctx, "web", 100*time.Millisecond); close(done) }()
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("waitUntilRunStarted took %s, want close to its 100ms timeout", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitUntilRunStarted did not return once its timeout passed")
	}
}

// waitUntilRunStarted returns as soon as the Deployment is gone, rather than
// waiting out its full timeout.
func TestWaitUntilRunStartedReturnsWhenDeploymentIsGone(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	id, _, err := a.CreateContainer(ctx, RunOptions{Name: "web", Image: "nginx"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := client.AppsV1().Deployments(testNamespace).Delete(ctx, "web", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { a.waitUntilRunStarted(ctx, "web", 5*time.Second); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitUntilRunStarted did not return once the deployment was gone")
	}
}

// waitUntilRunStarted returns as soon as listing the container's pods fails,
// rather than waiting out its full timeout.
func TestWaitUntilRunStartedReturnsOnListError(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	id, _, err := a.CreateContainer(ctx, RunOptions{Name: "web", Image: "nginx"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("list failed")
	})
	done := make(chan struct{})
	go func() { a.waitUntilRunStarted(ctx, "web", 5*time.Second); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitUntilRunStarted did not return on a list error")
	}
}

// annotate retries once a conflicting update is resolved, rather than
// failing the whole operation on the first stale write.
func TestAnnotateRetriesOnConflict(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	if _, err := client.AppsV1().Deployments(testNamespace).Create(ctx, deployment("web", "u1", managed()), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	client.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts == 1 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "deployments"}, "web", errors.New("conflict"))
		}
		return false, nil, nil
	})
	v := "x"
	if err := a.annotate(ctx, "web", map[string]*string{"k": &v}); err != nil {
		t.Fatalf("annotate error = %v, want nil after retrying the conflict", err)
	}
	if attempts < 2 {
		t.Errorf("update attempts = %d, want at least 2 (one conflict, one retry)", attempts)
	}
}

// annotate reports a non-conflict update error instead of retrying it.
func TestAnnotateReturnsNonConflictError(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	if _, err := client.AppsV1().Deployments(testNamespace).Create(ctx, deployment("web", "u1", managed()), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	v := "x"
	if err := a.annotate(ctx, "web", map[string]*string{"k": &v}); err == nil || err.Error() != "boom" {
		t.Errorf("annotate error = %v, want \"boom\"", err)
	}
}

// Two starts of a run-once container at once each see no pod running: they
// still run it once. The pod is named after the version of the container
// both read, so only one pod is created.
func TestStartPodRunsOnceForConcurrentStarts(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	id, _, err := a.CreateContainer(ctx, RunOptions{Name: "job", Image: "busybox", RestartPolicy: "no"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := client.AppsV1().Deployments(testNamespace).Get(ctx, "job", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Both starts list the pods before either creates one.
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.PodList{}, nil
	})
	first, second := d.DeepCopy(), d.DeepCopy()
	if err := a.startPod(ctx, first); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if err := a.startPod(ctx, second); err != nil {
		t.Fatalf("second start: %v", err)
	}

	pods, err := client.Tracker().List(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, testNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(pods.(*corev1.PodList).Items); n != 1 {
		t.Errorf("%s has %d pods after two concurrent starts, want 1", id, n)
	}
}
