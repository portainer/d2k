package router

import (
	"encoding/json"
	"net/http"
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
