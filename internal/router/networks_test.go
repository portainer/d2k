package router

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"
)

// TestNetworkDriverAndScope checks that a network reports the driver it was
// created with, and the scope Docker gives that driver. Portainer offers only
// networks of scope "local" to a container outside Swarm, and compose creates
// its networks without a driver: bridge, as in Docker.
func TestNetworkDriverAndScope(t *testing.T) {
	h, _ := newTestRouter()
	for _, tt := range []struct {
		name, body, driver, scope string
	}{
		{"app_default", `{"Name":"app_default"}`, "bridge", "local"},
		{"backend", `{"Name":"backend","Driver":"bridge"}`, "bridge", "local"},
		{"stack_net", `{"Name":"stack_net","Driver":"overlay"}`, "overlay", "swarm"},
	} {
		if rec := do(t, h, "POST", "/networks/create", tt.body); rec.Code != http.StatusCreated {
			t.Fatalf("network create %s = %d %s", tt.name, rec.Code, rec.Body)
		}

		rec := do(t, h, "GET", "/networks/"+tt.name, "")
		var inspected struct{ Driver, Scope string }
		if err := json.Unmarshal(rec.Body.Bytes(), &inspected); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("network inspect %s = %d %s", tt.name, rec.Code, rec.Body)
		}
		if inspected.Driver != tt.driver || inspected.Scope != tt.scope {
			t.Errorf("inspect %s: driver %q scope %q, want %q %q", tt.name, inspected.Driver, inspected.Scope, tt.driver, tt.scope)
		}

		rec = do(t, h, "GET", "/networks", "")
		var list []struct{ Name, Driver, Scope string }
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		for _, n := range list {
			if n.Name == tt.name && (n.Driver != tt.driver || n.Scope != tt.scope) {
				t.Errorf("list %s: driver %q scope %q, want %q %q", tt.name, n.Driver, n.Scope, tt.driver, tt.scope)
			}
		}
	}
}

// TestNetworkListFilters checks the filters of docker network ls, matched as
// the daemon does. A filter d2k ignored listed every network, which a
// `docker network ls -q -f ... | xargs docker network rm` would then remove.
func TestNetworkListFilters(t *testing.T) {
	h, _ := newTestRouter()
	for _, body := range []string{`{"Name":"app_default","Labels":{"tier":"web"}}`, `{"Name":"spare"}`} {
		if rec := do(t, h, "POST", "/networks/create", body); rec.Code != http.StatusCreated {
			t.Fatalf("network create = %d %s", rec.Code, rec.Body)
		}
	}
	if rec := do(t, h, "POST", "/containers/create?name=web", `{"Image":"nginx","HostConfig":{"NetworkMode":"app_default"}}`); rec.Code != http.StatusCreated {
		t.Fatalf("container create = %d %s", rec.Code, rec.Body)
	}

	list := func(filters string) []string {
		rec := do(t, h, "GET", "/networks?filters="+url.QueryEscape(filters), "")
		if rec.Code != http.StatusOK {
			t.Errorf("filters %s: status = %d %s", filters, rec.Code, rec.Body)
			return nil
		}
		var nets []struct{ Name string }
		if err := json.Unmarshal(rec.Body.Bytes(), &nets); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, n := range nets {
			names = append(names, n.Name)
		}
		return names
	}
	for _, tt := range []struct {
		filters       string
		want, wantNot []string
	}{
		{`{"name":["spare"]}`, []string{"spare"}, []string{"app_default", "bridge"}},
		{`{"id":["d2k-d2k-spare"]}`, []string{"spare"}, []string{"app_default", "bridge"}},
		{`{"label":["tier=web"]}`, []string{"app_default"}, []string{"spare", "bridge"}},
		{`{"driver":["bridge"]}`, []string{"bridge", "app_default", "spare"}, []string{"host", "none"}},
		{`{"type":["custom"]}`, []string{"app_default", "spare"}, []string{"bridge", "host", "none"}},
		{`{"type":["builtin"]}`, []string{"bridge", "host", "none"}, []string{"app_default", "spare"}},
		{`{"dangling":["true"]}`, []string{"spare"}, []string{"app_default", "bridge"}},
		{`{"dangling":["false"]}`, []string{"app_default", "bridge"}, []string{"spare"}},
	} {
		got := list(tt.filters)
		for _, name := range tt.want {
			if !slices.Contains(got, name) {
				t.Errorf("filters %s: %v lacks %s", tt.filters, got, name)
			}
		}
		for _, name := range tt.wantNot {
			if slices.Contains(got, name) {
				t.Errorf("filters %s: %v has %s", tt.filters, got, name)
			}
		}
	}

	for _, filters := range []string{`{"color":["red"]}`, `{"dangling":["maybe"]}`, `{"type":["other"]}`} {
		if rec := do(t, h, "GET", "/networks?filters="+url.QueryEscape(filters), ""); rec.Code != http.StatusBadRequest {
			t.Errorf("filters %s: status = %d, want 400: %s", filters, rec.Code, rec.Body)
		}
	}
}
