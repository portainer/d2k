package containers

import (
	"slices"
	"testing"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/internal/filters"
)

func TestFilterContainers(t *testing.T) {
	ctrs := []adapter.ContainerSummary{
		{ID: "aaa111", Names: []string{"/proj-web-1"}, State: "running",
			Labels: map[string]string{"com.docker.compose.project": "proj"}},
		{ID: "bbb222", Names: []string{"/other-web-1"}, State: "exited",
			Labels: map[string]string{"com.docker.compose.project": "other"}},
	}

	tests := []struct {
		name string
		args filters.Args
		want []string
	}{
		{name: "no filters", args: filters.NewArgs(), want: []string{"aaa111", "bbb222"}},
		{name: "compose project label", args: filters.NewArgs(filters.Arg("label", "com.docker.compose.project=proj")), want: []string{"aaa111"}},
		{name: "label key only", args: filters.NewArgs(filters.Arg("label", "com.docker.compose.project")), want: []string{"aaa111", "bbb222"}},
		{name: "unknown label value", args: filters.NewArgs(filters.Arg("label", "com.docker.compose.project=nope")), want: nil},
		{name: "name without slash", args: filters.NewArgs(filters.Arg("name", "proj-web-1")), want: []string{"aaa111"}},
		{name: "id prefix", args: filters.NewArgs(filters.Arg("id", "bbb")), want: []string{"bbb222"}},
		{name: "status", args: filters.NewArgs(filters.Arg("status", "exited")), want: []string{"bbb222"}},
		{name: "keys are ANDed", args: filters.NewArgs(filters.Arg("status", "running"), filters.Arg("label", "com.docker.compose.project=other")), want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, c := range filterContainers(ctrs, tt.args) {
				got = append(got, c.ID)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("filterContainers() = %v, want %v", got, tt.want)
			}
		})
	}
}
