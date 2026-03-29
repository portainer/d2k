package portmapper

import (
	"testing"
)

func TestResolve_NoFlags(t *testing.T) {
	kind, mappings, _, err := Resolve(Request{})
	if err != nil {
		t.Fatal(err)
	}
	if kind != NoService {
		t.Errorf("expected NoService, got %d", kind)
	}
	if len(mappings) != 0 {
		t.Errorf("expected no mappings, got %d", len(mappings))
	}
}

func TestResolve_PublishAll(t *testing.T) {
	kind, mappings, _, err := Resolve(Request{
		PublishAll:   true,
		ExposedPorts: map[string]struct{}{"80/tcp": {}, "443/tcp": {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if kind != NodePortService {
		t.Errorf("expected NodePortService, got %d", kind)
	}
	if len(mappings) != 2 {
		t.Errorf("expected 2 mappings, got %d", len(mappings))
	}
	for _, m := range mappings {
		if m.HostPort != 0 {
			t.Errorf("NodePort mappings should have HostPort=0, got %d", m.HostPort)
		}
	}
}

func TestResolve_ExplicitMapping(t *testing.T) {
	kind, mappings, warnings, err := Resolve(Request{
		PortBindings: []string{"8080:80", "443:443/tcp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if kind != LoadBalancerService {
		t.Errorf("expected LoadBalancerService, got %d", kind)
	}
	if len(mappings) != 2 {
		t.Errorf("expected 2 mappings, got %d", len(mappings))
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got %v", warnings)
	}
}

func TestResolve_HostIPIgnored(t *testing.T) {
	kind, mappings, warnings, err := Resolve(Request{
		PortBindings: []string{"127.0.0.1:8080:80"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if kind != LoadBalancerService {
		t.Errorf("expected LoadBalancerService, got %d", kind)
	}
	if len(mappings) != 1 {
		t.Errorf("expected 1 mapping, got %d", len(mappings))
	}
	if mappings[0].HostPort != 8080 || mappings[0].ContainerPort != 80 {
		t.Errorf("unexpected mapping: %+v", mappings[0])
	}
	if len(warnings) != 1 {
		t.Errorf("expected 1 warning for ignored host IP, got %d", len(warnings))
	}
}
