package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/internal/apispec"
)

// loadSpec returns the pinned Docker Engine API specification as OpenAPI 3.
func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	data, err := os.ReadFile("../apispec/docker-v1.52.yaml")
	if err != nil {
		t.Fatal(err)
	}
	data, err = yaml.YAMLToJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	var doc2 openapi2.T
	if err := json.Unmarshal(data, &doc2); err != nil {
		t.Fatal(err)
	}
	doc, err := openapi2conv.ToV3(&doc2)
	if err != nil {
		t.Fatal(err)
	}
	// The daemon encodes unset slices and maps as null, but the spec marks no
	// field nullable. Accept null everywhere, as real Docker responses need.
	visited := map[*openapi3.Schema]bool{}
	for _, ref := range doc.Components.Schemas {
		allowNull(ref, visited)
	}
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			for _, resp := range op.Responses.Map() {
				for _, media := range resp.Value.Content {
					allowNull(media.Schema, visited)
				}
			}
		}
	}
	return doc
}

func allowNull(ref *openapi3.SchemaRef, visited map[*openapi3.Schema]bool) {
	if ref == nil || ref.Value == nil || visited[ref.Value] {
		return
	}
	s := ref.Value
	visited[s] = true
	s.Nullable = true
	for _, prop := range s.Properties {
		allowNull(prop, visited)
	}
	allowNull(s.Items, visited)
	if s.AdditionalProperties.Schema != nil {
		allowNull(s.AdditionalProperties.Schema, visited)
	}
	for _, refs := range [][]*openapi3.SchemaRef{s.AllOf, s.AnyOf, s.OneOf} {
		for _, sub := range refs {
			allowNull(sub, visited)
		}
	}
}

// conforms checks a JSON response against the spec's schema for the
// operation and status code.
func conforms(doc *openapi3.T, method, path string, status int, body []byte) error {
	route, ok := apispec.Match(method, path)
	if !ok {
		return fmt.Errorf("%s %s is not a Docker Engine API operation", method, path)
	}
	op := doc.Paths.Find(route.Path).GetOperation(method)
	resp := op.Responses.Status(status)
	if resp == nil {
		return fmt.Errorf("status %d is not a documented response of %s %s", status, method, route.Path)
	}
	media := resp.Value.Content.Get("application/json")
	if media == nil || media.Schema == nil {
		return nil
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return fmt.Errorf("response is not JSON: %w", err)
	}
	return media.Schema.Value.VisitJSON(value, openapi3.MultiErrors())
}

// newTestRouter returns the d2k router backed by a fake cluster.
func newTestRouter() (http.Handler, *fake.Clientset) {
	client := fake.NewClientset()
	// Assign UIDs on create, as the API server does; container IDs derive from them.
	client.PrependReactor("create", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if obj, err := meta.Accessor(action.(k8stesting.CreateAction).GetObject()); err == nil && obj.GetUID() == "" {
			obj.SetUID(uuid.NewUUID())
		}
		return false, nil, nil
	})
	return New(adapter.NewWithClient(client, "d2k"), "d2k", false), client
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/v"+apispec.Version+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestContract checks d2k's responses against the Docker Engine API schema.
// Known violations name the phase that fixes them; the test fails when a
// known violation is fixed, so the case can be promoted.
func TestContract(t *testing.T) {
	doc := loadSpec(t)
	h, _ := newTestRouter()

	create := do(t, h, "POST", "/containers/create?name=web", `{"Image":"nginx","Labels":{"app.tier":"web"}}`)
	var created struct{ ID string }
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil || create.Code != http.StatusCreated {
		t.Fatalf("container create = %d %s", create.Code, create.Body)
	}
	if rec := do(t, h, "POST", "/volumes/create", `{"Name":"data"}`); rec.Code != http.StatusCreated {
		t.Fatalf("volume create = %d %s", rec.Code, rec.Body)
	}

	tests := []struct {
		method, path string
		status       int
		violation    string // known violation and the phase that fixes it
	}{
		{"GET", "/version", 200, ""},
		{"GET", "/info", 200, ""},
		{"GET", "/containers/json?all=1", 200, ""},
		{"GET", "/containers/" + created.ID + "/json", 200, ""},
		{"GET", "/containers/nope/json", 404, ""},
		{"GET", "/images/json", 200, "Phase 5: required image summary fields missing"},
		{"GET", "/networks", 200, ""},
		{"GET", "/networks/bridge", 200, ""},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := do(t, h, tt.method, tt.path, "")
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.status, rec.Body)
			}
			path, _, _ := strings.Cut(tt.path, "?")
			err := conforms(doc, tt.method, path, rec.Code, rec.Body.Bytes())
			switch {
			case tt.violation == "" && err != nil:
				t.Errorf("response does not match the spec: %v", err)
			case tt.violation != "" && err == nil:
				t.Errorf("known violation (%s) is fixed: clear it", tt.violation)
			}
		})
	}
}

// TestUnsupportedOperations checks that Docker Engine API operations d2k does
// not implement answer 501, and that other paths answer 404.
func TestUnsupportedOperations(t *testing.T) {
	h, _ := newTestRouter()
	for _, tt := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/system/df", http.StatusNotImplemented},
		{"POST", "/build", http.StatusNotImplemented},
		{"POST", "/containers/web/pause", http.StatusNotImplemented},
		{"GET", "/containers/web/top", http.StatusNotImplemented},
		{"GET", "/images/library/nginx:latest/history", http.StatusNotImplemented},
		// Swarm operations when swarm mode is off.
		{"GET", "/services", http.StatusNotImplemented},
		{"GET", "/not/docker", http.StatusNotFound},
	} {
		rec := do(t, h, tt.method, tt.path, "")
		if rec.Code != tt.status {
			t.Errorf("%s %s = %d, want %d: %s", tt.method, tt.path, rec.Code, tt.status, rec.Body)
		}
		var body struct{ Message string }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Message == "" {
			t.Errorf("%s %s body = %q, want a Docker error {\"message\": ...}", tt.method, tt.path, rec.Body)
		}
	}
}
