package volumes

import (
	"slices"
	"testing"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/internal/filters"
)

func TestFilterVolumes(t *testing.T) {
	vols := []adapter.VolumeSummary{
		{Name: "proj-data", Driver: "d2k", Labels: map[string]string{"com.docker.compose.project": "proj"}},
		{Name: "other-data", Driver: "d2k", Labels: map[string]string{"com.docker.compose.project": "other"}},
	}

	tests := []struct {
		name string
		args filters.Args
		want []string
	}{
		{name: "no filters", args: filters.NewArgs(), want: []string{"proj-data", "other-data"}},
		{name: "compose project label", args: filters.NewArgs(filters.Arg("label", "com.docker.compose.project=proj")), want: []string{"proj-data"}},
		{name: "name", args: filters.NewArgs(filters.Arg("name", "other-data")), want: []string{"other-data"}},
		{name: "driver", args: filters.NewArgs(filters.Arg("driver", "local")), want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, v := range filterVolumes(vols, tt.args) {
				got = append(got, v.Name)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("filterVolumes() = %v, want %v", got, tt.want)
			}
		})
	}
}
