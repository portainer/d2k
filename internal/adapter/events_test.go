package adapter

import (
	"slices"
	"testing"
	"time"

	"github.com/moby/moby/api/types/events"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/portainer/d2k/internal/types"
)

func eventDeployment(replicas int32, annotations map[string]string) *appsv1.Deployment {
	d := deployment("web-k8s", "11111111-0000-4000-8000-000000000001", managed())
	d.Spec.Replicas = &replicas
	d.Annotations = map[string]string{
		types.AnnotationDockerName:   "web",
		types.AnnotationImageRef:     "nginx:alpine",
		types.AnnotationDockerLabels: `{"com.docker.compose.project":"demo"}`,
	}
	for k, v := range annotations {
		d.Annotations[k] = v
	}
	return d
}

func actions(msgs []events.Message) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, string(m.Type)+" "+string(m.Action))
	}
	return out
}

func TestDeploymentEvents(t *testing.T) {
	known := map[string]*appsv1.Deployment{}
	now := time.Now()

	created := deploymentEvents(known, watch.Added, eventDeployment(0, nil), now)
	if got := actions(created); !slices.Equal(got, []string{"container create"}) {
		t.Fatalf("added = %q", got)
	}
	m := created[0]
	if m.Actor.ID != containerID("11111111-0000-4000-8000-000000000001") || m.Actor.Attributes["name"] != "web" ||
		m.Actor.Attributes["image"] != "nginx:alpine" || m.Actor.Attributes["com.docker.compose.project"] != "demo" || m.Scope != "local" {
		t.Fatalf("create event = %+v", m)
	}

	for _, tt := range []struct {
		name string
		next *appsv1.Deployment
		want []string
	}{
		{"start", eventDeployment(1, nil), []string{"container start"}},
		{"unrelated change", eventDeployment(1, map[string]string{"x": "y"}), nil},
		{"stop", eventDeployment(0, nil), []string{"container stop"}},
		{"rename", eventDeployment(0, map[string]string{types.AnnotationDockerName: "web2"}), []string{"container rename"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := actions(deploymentEvents(known, watch.Modified, tt.next, now)); !slices.Equal(got, tt.want) {
				t.Fatalf("events = %q, want %q", got, tt.want)
			}
		})
	}
	renamed := deploymentEvents(known, watch.Modified, eventDeployment(0, map[string]string{types.AnnotationDockerName: "web3"}), now)
	if renamed[0].Actor.Attributes["oldName"] != "/web2" || renamed[0].Actor.Attributes["name"] != "web3" {
		t.Fatalf("rename attributes = %v", renamed[0].Actor.Attributes)
	}
	if got := actions(deploymentEvents(known, watch.Deleted, eventDeployment(0, nil), now)); !slices.Equal(got, []string{"container destroy"}) {
		t.Fatalf("deleted = %q", got)
	}
}

func TestPodEventsReportDeaths(t *testing.T) {
	known := map[string]*appsv1.Deployment{"web-k8s": eventDeployment(1, nil)}
	restarts := map[string]int32{}
	pod := func(restarts int32, exit int32) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-k8s-abc", Labels: map[string]string{"app": "web-k8s"}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				RestartCount:         restarts,
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit}},
			}}},
		}
	}
	if got := podEvents(known, restarts, map[string]int32{}, watch.Added, pod(0, 0), time.Now()); got != nil {
		t.Fatalf("new pod = %q, want no event", actions(got))
	}
	died := podEvents(known, restarts, map[string]int32{}, watch.Modified, pod(1, 137), time.Now())
	if got := actions(died); !slices.Equal(got, []string{"container die"}) || died[0].Actor.Attributes["exitCode"] != "137" {
		t.Fatalf("restart = %q %v", got, died)
	}
	if got := podEvents(known, restarts, map[string]int32{}, watch.Modified, pod(1, 137), time.Now()); got != nil {
		t.Fatalf("same restart count = %q, want no event", actions(got))
	}
}

func TestEventHub(t *testing.T) {
	var hub eventHub
	for i := range eventHistorySize + 10 {
		hub.publish(events.Message{Action: events.Action("e"), TimeNano: int64(i)})
	}
	history, live, cancel := hub.subscribe()
	if len(history) != eventHistorySize || history[0].TimeNano != 10 {
		t.Fatalf("history has %d events from %d, want %d from 10", len(history), history[0].TimeNano, eventHistorySize)
	}
	hub.publish(events.Message{Action: "live"})
	if m := <-live; m.Action != "live" {
		t.Fatalf("live = %+v", m)
	}
	cancel()
	cancel()                                     // idempotent
	hub.publish(events.Message{Action: "after"}) // no subscriber, must not block
}
