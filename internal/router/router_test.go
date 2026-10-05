package router

import "testing"

func TestStripAPIVersion(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: "/v1.41/containers/json", want: "/containers/json"},
		{path: "/v1.47/volumes/create", want: "/volumes/create"},
		// Unversioned paths that merely start with "v" must be left alone.
		{path: "/volumes/create", want: "/volumes/create"},
		{path: "/volumes/mydata", want: "/volumes/mydata"},
		{path: "/version", want: "/version"},
		{path: "/containers/json", want: "/containers/json"},
	}
	for _, tt := range tests {
		if got := stripAPIVersion(tt.path); got != tt.want {
			t.Errorf("stripAPIVersion(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}
