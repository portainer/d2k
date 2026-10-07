package adapter

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/moby/moby/api/types/events"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
