package router

import (
	"errors"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

// TestErrorStatus checks that failures map to the status the Docker daemon
// uses, which clients branch on (compose creates a volume on 404, treats a
// container as gone on 404, reports conflicts on 409).
func TestErrorStatus(t *testing.T) {
	h, _ := newTestRouter()
	if rec := do(t, h, "POST", "/containers/create?name=web", `{"Image":"nginx"}`); rec.Code != http.StatusCreated {
		t.Fatalf("container create = %d %s", rec.Code, rec.Body)
	}

	for _, tt := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/containers/nope/json", "", http.StatusNotFound},
		{"POST", "/containers/nope/start", "", http.StatusNotFound},
		{"POST", "/containers/nope/stop", "", http.StatusNotFound},
		{"DELETE", "/containers/nope", "", http.StatusNotFound},
		{"POST", "/containers/nope/rename?name=x", "", http.StatusNotFound},
		{"POST", "/containers/create?name=web", `{"Image":"nginx"}`, http.StatusConflict},
		{"POST", "/containers/create?name=bad", `{"Image":"nginx","HostConfig":{"Binds":["novolumepath"]}}`, http.StatusBadRequest},
		{"POST", "/containers/create?name=bad", `{not json`, http.StatusBadRequest},
		{"GET", "/volumes/nope", "", http.StatusNotFound},
		{"POST", "/volumes/create", `{"Name":""}`, http.StatusBadRequest},
		{"DELETE", "/networks/bridge", "", http.StatusForbidden},
		{"GET", "/containers/json?filters={bad", "", http.StatusBadRequest},
	} {
		if rec := do(t, h, tt.method, tt.path, tt.body); rec.Code != tt.want {
			t.Errorf("%s %s = %d, want %d: %s", tt.method, tt.path, rec.Code, tt.want, rec.Body)
		}
	}
}

// TestClusterErrorStatus checks how Kubernetes API failures surface.
func TestClusterErrorStatus(t *testing.T) {
	gr := schema.GroupResource{Group: "apps", Resource: "deployments"}
	for _, tt := range []struct {
		name string
		err  error
		want int
	}{
		{"quota rejection", apierrors.NewForbidden(gr, "web", errors.New("exceeded quota")), http.StatusForbidden},
		{"API server unavailable", apierrors.NewServiceUnavailable("etcd"), http.StatusServiceUnavailable},
		{"unexpected failure", errors.New("connection refused"), http.StatusInternalServerError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, client := newTestRouter()
			client.PrependReactor("*", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tt.err
			})
			if rec := do(t, h, "POST", "/containers/create?name=web", `{"Image":"nginx"}`); rec.Code != tt.want {
				t.Errorf("container create = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
			if rec := do(t, h, "GET", "/containers/json", ""); rec.Code != tt.want {
				t.Errorf("container list = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}
