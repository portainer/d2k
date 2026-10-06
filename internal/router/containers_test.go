package router

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// TestContainerListByNetworkAndVolume checks the network and volume filters
// Portainer's network and volume pages send to list the containers using
// them. As in the daemon, network matches a network's name or an ID prefix,
// and volume a volume's name or a mount's destination.
func TestContainerListByNetworkAndVolume(t *testing.T) {
	h, _ := newTestRouter()
	var net struct{ ID string }
	rec := do(t, h, "POST", "/networks/create", `{"Name":"backend"}`)
	if err := json.Unmarshal(rec.Body.Bytes(), &net); err != nil || rec.Code != http.StatusCreated {
		t.Fatalf("network create = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/volumes/create", `{"Name":"data"}`); rec.Code != http.StatusCreated {
		t.Fatalf("volume create = %d %s", rec.Code, rec.Body)
	}
	for name, body := range map[string]string{
		"db":  `{"Image":"postgres","HostConfig":{"NetworkMode":"backend","Binds":["data:/var/lib/postgresql/data"]}}`,
		"web": `{"Image":"nginx"}`,
	} {
		if rec := do(t, h, "POST", "/containers/create?name="+name, body); rec.Code != http.StatusCreated {
			t.Fatalf("container create %s = %d %s", name, rec.Code, rec.Body)
		}
	}

	for _, tt := range []struct {
		filters string
		want    []string
	}{
		{`{"network":["backend"]}`, []string{"/db"}},
		{`{"network":["` + net.ID + `"]}`, []string{"/db"}},
		{`{"network":["` + net.ID[:12] + `"]}`, []string{"/db"}},
		{`{"network":["frontend"]}`, nil},
		{`{"volume":["data"]}`, []string{"/db"}},
		{`{"volume":["/var/lib/postgresql/data"]}`, []string{"/db"}},
		{`{"volume":["other"]}`, nil},
	} {
		rec := do(t, h, "GET", "/containers/json?all=1&filters="+url.QueryEscape(tt.filters), "")
		if rec.Code != http.StatusOK {
			t.Errorf("filters %s: status = %d, want 200: %s", tt.filters, rec.Code, rec.Body)
			continue
		}
		var list []struct{ Names []string }
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, c := range list {
			got = append(got, strings.Join(c.Names, ","))
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("filters %s: containers = %v, want %v", tt.filters, got, tt.want)
		}
	}
}
