package adapter

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/moby/moby/api/types/events"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// docker rm -f of a running container kills it: as in Docker, it dies,
// with 137, before it is destroyed. Attached compose up waits for that die
// (D2K-072).
func TestEventsReportARemovedRunningContainerDead(t *testing.T) {
	for _, policy := range []string{"always", "no"} {
		t.Run(policy, func(t *testing.T) {
			a, client := newTestAdapter(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			id, _, err := a.CreateContainer(ctx, RunOptions{Name: "web", Image: "nginx", RestartPolicy: policy})
			if err != nil {
				t.Fatal(err)
			}
			if err := a.StartContainer(ctx, id); err != nil {
				t.Fatal(err)
			}
			pods := client.CoreV1().Pods(testNamespace)
			if list, err := pods.List(ctx, metav1.ListOptions{LabelSelector: "app=web"}); err != nil || len(list.Items) == 0 {
				p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-a", Namespace: testNamespace, Labels: managedLabels("web")}}
				if _, err := pods.Create(ctx, p, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			setStatus(t, a, podOf(t, a, "web"), corev1.PodRunning, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})

			_, live, unsubscribe := a.SubscribeEvents()
			defer unsubscribe()
			go a.RecordEvents(ctx)
			time.Sleep(200 * time.Millisecond)
			if err := a.RemoveContainer(ctx, id); err != nil {
				t.Fatal(err)
			}

			var got []string
			deadline := time.After(5 * time.Second)
			for {
				select {
				case m := <-live:
					if m.Actor.ID != id {
						continue
					}
					got = append(got, string(m.Action)+" "+m.Actor.Attributes["exitCode"])
					if m.Action == events.ActionDestroy {
						if want := []string{"die 137", "destroy "}; !slices.Equal(got, want) {
							t.Fatalf("events = %q, want %q", got, want)
						}
						return
					}
				case <-deadline:
					t.Fatalf("events = %q, then nothing", got)
				}
			}
		})
	}
}

// A run-once container's pods are deleted before its Deployment, and the
// kubelet ends a deleted pod's container a while later, after the
// Deployment is gone: its death is still reported, before its destroy.
func TestEventsReportARemovedRunOnceContainerDeadWhilePodsEndGracefully(t *testing.T) {
	for _, policy := range []string{"no", "on-failure"} {
		t.Run(policy, func(t *testing.T) {
			a, client := newTestAdapter(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			id, _, err := a.CreateContainer(ctx, RunOptions{Name: "web", Image: "nginx", RestartPolicy: policy})
			if err != nil {
				t.Fatal(err)
			}
			if err := a.StartContainer(ctx, id); err != nil {
				t.Fatal(err)
			}
			setStatus(t, a, podOf(t, a, "web"), corev1.PodRunning, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})

			// As the API server does, a delete with a grace period marks the
			// pod; the kubelet ends its container, then the pod goes.
			podsGVR := corev1.SchemeGroupVersion.WithResource("pods")
			client.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				del := action.(k8stesting.DeleteAction)
				obj, err := client.Tracker().Get(podsGVR, testNamespace, del.GetName())
				if err != nil {
					return false, nil, nil
				}
				p := obj.(*corev1.Pod).DeepCopy()
				if p.DeletionTimestamp != nil {
					return true, nil, nil
				}
				p.DeletionTimestamp, p.DeletionGracePeriodSeconds = ptr(metav1.Now()), del.GetDeleteOptions().GracePeriodSeconds
				if err := client.Tracker().Update(podsGVR, p, testNamespace); err != nil {
					return true, nil, err
				}
				go func() {
					time.Sleep(300 * time.Millisecond)
					p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}}}}
					_ = client.Tracker().Update(podsGVR, p, testNamespace)
					time.Sleep(100 * time.Millisecond)
					_ = client.Tracker().Delete(podsGVR, testNamespace, p.Name)
				}()
				return true, nil, nil
			})

			_, live, unsubscribe := a.SubscribeEvents()
			defer unsubscribe()
			go a.RecordEvents(ctx)
			time.Sleep(200 * time.Millisecond)
			if err := a.RemoveContainer(ctx, id); err != nil {
				t.Fatal(err)
			}

			var got []string
			deadline := time.After(2 * time.Second)
			for {
				select {
				case m := <-live:
					if m.Actor.ID == id {
						got = append(got, string(m.Action)+" "+m.Actor.Attributes["exitCode"])
					}
				case <-deadline:
					if want := []string{"die 137", "destroy "}; !slices.Equal(got, want) {
						t.Fatalf("events = %q, want %q", got, want)
					}
					return
				}
			}
		})
	}
}
