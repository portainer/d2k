package router

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

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

	pull := do(t, h, "POST", "/images/create?fromImage=nginx&tag=alpine", "").Body.String()
	if strings.Contains(pull, "Digest") || strings.Contains(pull, "Pull complete") || !strings.Contains(pull, "nginx:alpine") {
		t.Errorf("pull stream = %s, want an acknowledgement without a made-up pull", pull)
	}
}
