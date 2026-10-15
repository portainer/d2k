package apispec

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMatch(t *testing.T) {
	tests := []struct {
		method, path string
		want         string
	}{
		{"GET", "/_ping", "/_ping"},
		{"HEAD", "/_ping", "/_ping"},
		{"GET", "/containers/json", "/containers/json"},
		{"GET", "/containers/0ec2969b-083/json", "/containers/{id}/json"},
		{"POST", "/containers/web/pause", "/containers/{id}/pause"},
		{"DELETE", "/containers/web", "/containers/{id}"},
		{"POST", "/containers/create", "/containers/create"},
		// Image names contain slashes and colons.
		{"GET", "/images/library/nginx:latest/json", "/images/{name}/json"},
		{"DELETE", "/images/ghcr.io/org/app:1.0", "/images/{name}"},
		{"GET", "/system/df", "/system/df"},
		{"GET", "/volumes/data", "/volumes/{name}"},
		// Not Docker Engine API operations.
		{"GET", "/foo", ""},
		{"PUT", "/containers/json", ""},
		{"GET", "/containers/web/bogus", ""},
	}
	for _, tt := range tests {
		got, ok := Match(tt.method, tt.path)
		if got.Path != tt.want || ok != (tt.want != "") {
			t.Errorf("Match(%s %s) = %q, %v; want %q", tt.method, tt.path, got.Path, ok, tt.want)
		}
	}
}

func TestNotRouted(t *testing.T) {
	for path, want := range map[string]int{
		"/system/df":             http.StatusNotImplemented,
		"/containers/web/pause":  http.StatusNotFound, // POST only
		"/not/a/docker/api/path": http.StatusNotFound,
	} {
		rec := httptest.NewRecorder()
		NotRouted(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want {
			t.Errorf("NotRouted(GET %s) = %d, want %d", path, rec.Code, want)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("NotRouted(GET %s) Content-Type = %q, want a Docker JSON error", path, ct)
		}
	}
}
