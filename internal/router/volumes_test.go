package router

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
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
