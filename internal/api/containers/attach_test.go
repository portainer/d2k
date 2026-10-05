package containers

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

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
