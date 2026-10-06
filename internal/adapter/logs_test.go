package adapter

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
