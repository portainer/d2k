package events

import (
	"testing"

	"github.com/moby/moby/api/types/events"

	"github.com/portainer/d2k/internal/filters"
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
