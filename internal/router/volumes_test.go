package router

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/volume"
)

// TestVolumeListDangling checks the dangling filter Portainer's volume list
// sends, twice, to mark the volumes no container uses.
func TestVolumeListDangling(t *testing.T) {
	h, _ := newTestRouter()
	for _, name := range []string{"used", "spare"} {
		if rec := do(t, h, "POST", "/volumes/create", `{"Name":"`+name+`"}`); rec.Code != http.StatusCreated {
			t.Fatalf("volume create %s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := do(t, h, "POST", "/containers/create?name=web", `{"Image":"nginx","HostConfig":{"Binds":["used:/data"]}}`); rec.Code != http.StatusCreated {
		t.Fatalf("container create = %d %s", rec.Code, rec.Body)
	}

	for _, tt := range []struct {
		dangling string
		want     []string
	}{
		{"false", []string{"used"}},
		{"true", []string{"spare"}},
		{"0", []string{"used"}},
		{"1", []string{"spare"}},
	} {
		rec := do(t, h, "GET", "/volumes?filters="+url.QueryEscape(`{"dangling":["`+tt.dangling+`"]}`), "")
		if rec.Code != http.StatusOK {
			t.Errorf("dangling=%s: status = %d, want 200: %s", tt.dangling, rec.Code, rec.Body)
			continue
		}
		var list volume.ListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, v := range list.Volumes {
			got = append(got, v.Name)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("dangling=%s: volumes = %v, want %v", tt.dangling, got, tt.want)
		}
	}

	if rec := do(t, h, "GET", "/volumes?filters="+url.QueryEscape(`{"dangling":["maybe"]}`), ""); rec.Code != http.StatusBadRequest {
		t.Errorf("dangling=maybe: status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

// TestVolumeCreateWithoutName checks that, as in Docker, a volume created
// without a name gets a random one and the anonymous label: docker volume
// create and Portainer's form leave the name empty.
func TestVolumeCreateWithoutName(t *testing.T) {
	h, _ := newTestRouter()
	rec := do(t, h, "POST", "/volumes/create", `{"Name":""}`)
	var vol volume.Volume
	if err := json.Unmarshal(rec.Body.Bytes(), &vol); err != nil || rec.Code != http.StatusCreated {
		t.Fatalf("volume create = %d %s, want 201", rec.Code, rec.Body)
	}
	if len(vol.Name) != 64 || strings.Trim(vol.Name, "0123456789abcdef") != "" {
		t.Errorf("name = %q, want 64 hex digits", vol.Name)
	}
	if _, ok := vol.Labels["com.docker.volume.anonymous"]; !ok {
		t.Errorf("labels = %v, want com.docker.volume.anonymous", vol.Labels)
	}
	if rec := do(t, h, "GET", "/volumes/"+vol.Name, ""); rec.Code != http.StatusOK {
		t.Errorf("inspect = %d %s", rec.Code, rec.Body)
	}
}

// TestVolumeRemove checks the status DELETE /volumes/{name} answers: 404 for
// a name that was never created, 409 while a container still uses it (even
// stopped, as Docker keeps it), and 204 once it's free.
func TestVolumeRemove(t *testing.T) {
	h, _ := newTestRouter()
	if rec := do(t, h, "DELETE", "/volumes/nope", ""); rec.Code != http.StatusNotFound {
		t.Errorf("remove missing = %d, want 404: %s", rec.Code, rec.Body)
	}

	if rec := do(t, h, "POST", "/volumes/create", `{"Name":"db_data"}`); rec.Code != http.StatusCreated {
		t.Fatalf("volume create = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/containers/create?name=db", `{"Image":"postgres","HostConfig":{"Binds":["db_data:/var/lib/postgresql/data"]}}`); rec.Code != http.StatusCreated {
		t.Fatalf("container create = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "DELETE", "/volumes/db_data", ""); rec.Code != http.StatusConflict {
		t.Errorf("remove in use = %d, want 409: %s", rec.Code, rec.Body)
	}

	if rec := do(t, h, "DELETE", "/containers/db", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("container remove = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "DELETE", "/volumes/db_data", ""); rec.Code != http.StatusNoContent {
		t.Errorf("remove free = %d, want 204: %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/volumes/db_data", ""); rec.Code != http.StatusNotFound {
		t.Errorf("inspect after remove = %d, want 404: %s", rec.Code, rec.Body)
	}
}
