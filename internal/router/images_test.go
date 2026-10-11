package router

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/image"
)

// d2k stores no images: the kubelet pulls them when a container starts. The
// image endpoints answer what compose and Portainer need without inventing
// image contents.
func TestImages(t *testing.T) {
	h, _ := newTestRouter()

	var list []image.Summary
	if rec := do(t, h, "GET", "/images/json", ""); json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list) != 0 {
		t.Errorf("image list = %s, want []", rec.Body)
	}

	inspect := func(ref string) image.InspectResponse {
		var resp image.InspectResponse
		if rec := do(t, h, "GET", "/images/"+ref+"/json", ""); rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
			t.Fatalf("inspect %s = %d %s", ref, rec.Code, rec.Body)
		}
		return resp
	}
	got := inspect("library/nginx:alpine")
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(got.ID) || got.ID != inspect("library/nginx:alpine").ID {
		t.Errorf("image ID = %q, want a stable sha256 digest", got.ID)
	}
	if got.ID == inspect("redis").ID {
		t.Error("different references share an image ID")
	}
	if got.Config != nil && (len(got.Config.Cmd) > 0 || len(got.Config.Entrypoint) > 0) {
		t.Errorf("config = %+v, want none: d2k does not know the image's", got.Config)
	}
	if _, err := time.Parse(time.RFC3339Nano, got.Created); err != nil {
		t.Errorf("Created = %q: %v (docker compose v2.40.3 fails parsing it as a timestamp)", got.Created, err)
	}

	pull := do(t, h, "POST", "/images/create?fromImage=nginx&tag=alpine", "").Body.String()
	if strings.Contains(pull, "Digest") || strings.Contains(pull, "Pull complete") || !strings.Contains(pull, "nginx:alpine") {
		t.Errorf("pull stream = %s, want an acknowledgement without a made-up pull", pull)
	}
}

// docker pull combines fromImage and tag as the Docker daemon does
// (moby daemon/server/router/image/image_routes.go postImagesCreate): tag
// replaces a tag in fromImage, a digest in tag replaces any tag or digest,
// and a pull needs a valid fromImage.
func TestPullReference(t *testing.T) {
	h, _ := newTestRouter()
	const dgst = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	for _, tc := range []struct{ query, want string }{
		{"fromImage=nginx&tag=alpine", "nginx:alpine"},
		{"fromImage=nginx:old&tag=new", "nginx:new"},
		{"fromImage=nginx:old&tag=" + dgst, "nginx@" + dgst},
		{"fromImage=nginx&tag=" + dgst, "nginx@" + dgst},
		{"fromImage=registry.example.com:5000/team/app&tag=1.2", "registry.example.com:5000/team/app:1.2"},
	} {
		rec := do(t, h, "POST", "/images/create?"+tc.query, "")
		var msg struct{ Status string }
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &msg) != nil || !strings.HasPrefix(msg.Status, tc.want+": ") {
			t.Errorf("pull %s = %d %s, want status for %s", tc.query, rec.Code, rec.Body, tc.want)
		}
	}
	for _, query := range []string{"tag=alpine", "fromImage=Not_Valid", "fromImage=nginx&tag=bad:tag"} {
		if rec := do(t, h, "POST", "/images/create?"+query, ""); rec.Code != 400 {
			t.Errorf("pull %s = %d %s, want 400", query, rec.Code, rec.Body)
		}
	}
}
