package events

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/events"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/internal/filters"
	"github.com/portainer/d2k/internal/types"
)

func TestMatches(t *testing.T) {
	ctr := events.Message{Type: events.ContainerEventType, Action: events.ActionStart, Actor: events.Actor{
		ID: "abc123", Attributes: map[string]string{"name": "web", "image": "nginx", "com.docker.compose.project": "demo"},
	}}
	vol := events.Message{Type: events.VolumeEventType, Action: events.ActionCreate, Actor: events.Actor{ID: "data"}}

	for _, tt := range []struct {
		name string
		args filters.Args
		ctr  bool
		vol  bool
	}{
		{"no filters", filters.NewArgs(), true, true},
		{"type", filters.NewArgs(filters.Arg("type", "container")), true, false},
		{"event", filters.NewArgs(filters.Arg("event", "create")), false, true},
		{"container by name", filters.NewArgs(filters.Arg("container", "web")), true, false},
		{"container by ID", filters.NewArgs(filters.Arg("container", "abc123")), true, false},
		{"compose project label", filters.NewArgs(filters.Arg("label", "com.docker.compose.project=demo")), true, false},
		{"image", filters.NewArgs(filters.Arg("image", "redis")), false, false},
		{"volume", filters.NewArgs(filters.Arg("volume", "data")), false, true},
	} {
		if got := matches(tt.args, ctr); got != tt.ctr {
			t.Errorf("%s: container event matches = %v, want %v", tt.name, got, tt.ctr)
		}
		if got := matches(tt.args, vol); got != tt.vol {
			t.Errorf("%s: volume event matches = %v, want %v", tt.name, got, tt.vol)
		}
	}
}

// As in Docker, until alone, or since=0, replays the recorded events.
func TestStreamReplaysWithUntilOrSinceZero(t *testing.T) {
	client := fake.NewClientset()
	a := adapter.NewWithClient(client, "ns")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.RecordEvents(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for i := 0; ; i++ {
		history, _, done := a.SubscribeEvents()
		done()
		if len(history) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("volume create event not recorded")
		}
		// Volumes that exist before the watch starts are not events: create
		// new ones until the watch reports one.
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("data-%d", i), Namespace: "ns", Labels: map[string]string{types.LabelManagedBy: types.LabelManagedByValue},
		}}
		_, _ = client.CoreV1().PersistentVolumeClaims("ns").Create(ctx, pvc, metav1.CreateOptions{})
		time.Sleep(20 * time.Millisecond)
	}
	until := fmt.Sprint(time.Now().Unix() + 1)
	time.Sleep(time.Until(time.Unix(time.Now().Unix()+1, 0)) + 10*time.Millisecond)

	h := NewHandler(a)
	for _, query := range []string{"until=" + until, "since=0&until=" + until} {
		rec := httptest.NewRecorder()
		h.Stream(rec, httptest.NewRequest("GET", "/events?"+query, nil))
		if !strings.Contains(rec.Body.String(), `"Action":"create"`) {
			t.Errorf("GET /events?%s = %q, want the recorded volume create", query, rec.Body.String())
		}
	}
}

// As in Docker, since after until is a bad request.
func TestStreamRefusesSinceAfterUntil(t *testing.T) {
	h := NewHandler(adapter.NewWithClient(fake.NewClientset(), "ns"))
	rec := httptest.NewRecorder()
	h.Stream(rec, httptest.NewRequest("GET", "/events?since=200&until=100", nil))
	if rec.Code != 400 {
		t.Errorf("GET /events?since=200&until=100 = %d %q, want 400", rec.Code, rec.Body.String())
	}
}
