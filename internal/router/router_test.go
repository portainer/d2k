package router

import (
	"archive/tar"
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

func TestAPIVersionRange(t *testing.T) {
	h, _ := newTestRouter()
	for _, tt := range []struct {
		path    string
		status  int
		message string
	}{
		{"/v1.41/_ping", http.StatusBadRequest, "client version 1.41 is too old. Minimum supported API version is 1.44, please upgrade your client to a newer version"},
		{"/v1.5/_ping", http.StatusBadRequest, "client version 1.5 is too old. Minimum supported API version is 1.44, please upgrade your client to a newer version"},
		{"/v1.53/_ping", http.StatusBadRequest, "client version 1.53 is too new. Maximum supported API version is 1.52"},
		{"/v1.44/_ping", http.StatusOK, ""},
		{"/v1.52/_ping", http.StatusOK, ""},
		{"/_ping", http.StatusOK, ""},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", tt.path, nil))
		if rec.Code != tt.status {
			t.Errorf("GET %s = %d, want %d: %s", tt.path, rec.Code, tt.status, rec.Body)
			continue
		}
		if tt.message != "" && !strings.Contains(rec.Body.String(), tt.message) {
			t.Errorf("GET %s body = %s, want %q", tt.path, rec.Body, tt.message)
		}
	}
}

func TestCreateWarnsAboutPlatform(t *testing.T) {
	h, _ := newTestRouter()
	rec := do(t, h, "POST", "/containers/create?name=web&platform=linux/arm64", `{"Image":"nginx"}`)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `platform \"linux/arm64\"`) {
		t.Fatalf("create = %d %s, want a warning about the platform", rec.Code, rec.Body)
	}
}

func TestPutArchive(t *testing.T) {
	h, _ := newTestRouter()
	if rec := do(t, h, "POST", "/containers/create?name=web", `{"Image":"nginx"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	_ = w.WriteHeader(&tar.Header{Name: "/run/secrets/pw", Mode: 0o444, Size: 2, Typeflag: tar.TypeReg})
	_, _ = w.Write([]byte("pw"))
	_ = w.Close()

	if rec := do(t, h, "PUT", "/containers/web/archive?path=/", b.String()); rec.Code != http.StatusOK {
		t.Errorf("put archive = %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := do(t, h, "PUT", "/containers/web/archive", b.String()); rec.Code != http.StatusBadRequest {
		t.Errorf("put archive without a path = %d, want 400", rec.Code)
	}
}
