package httputils

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/docker/docker/errdefs"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestParseFilters(t *testing.T) {
	accepted := map[string]bool{"label": true, "name": true}

	tests := []struct {
		name      string
		raw       string
		wantErr   bool
		wantLabel []string
	}{
		{name: "absent", raw: ""},
		{name: "map form", raw: `{"label":{"a=b":true}}`, wantLabel: []string{"a=b"}},
		{name: "legacy array form", raw: `{"label":["a=b"]}`, wantLabel: []string{"a=b"}},
		{name: "unsupported key", raw: `{"ancestor":{"nginx":true}}`, wantErr: true},
		{name: "invalid json", raw: `{label`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/containers/json?filters="+url.QueryEscape(tt.raw), nil)
			args, err := ParseFilters(r, accepted)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ParseFilters() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFilters() error: %v", err)
			}
			got := args.Get("label")
			if len(got) != len(tt.wantLabel) || (len(got) > 0 && got[0] != tt.wantLabel[0]) {
				t.Fatalf("label filter = %v, want %v", got, tt.wantLabel)
			}
		})
	}
}

func TestStatusFor(t *testing.T) {
	gr := schema.GroupResource{Group: "apps", Resource: "deployments"}
	tests := []struct {
		err  error
		want int
	}{
		{errdefs.NotFound(errors.New("container x not found")), http.StatusNotFound},
		{fmt.Errorf("inspect: %w", errdefs.NotFound(errors.New("x"))), http.StatusNotFound},
		{errdefs.Conflict(errors.New("name in use")), http.StatusConflict},
		{errdefs.InvalidParameter(errors.New("bad spec")), http.StatusBadRequest},
		{errdefs.Forbidden(errors.New("pre-defined network")), http.StatusForbidden},
		{errdefs.NotImplemented(errors.New("no")), http.StatusNotImplemented},
		// Kubernetes API errors passed through from the cluster.
		{fmt.Errorf("get: %w", apierrors.NewNotFound(gr, "web")), http.StatusNotFound},
		{apierrors.NewAlreadyExists(gr, "web"), http.StatusConflict},
		{apierrors.NewConflict(gr, "web", errors.New("modified")), http.StatusConflict},
		{apierrors.NewForbidden(gr, "web", errors.New("exceeded quota")), http.StatusForbidden},
		{apierrors.NewInvalid(schema.GroupKind{Kind: "Service"}, "1web", nil), http.StatusBadRequest},
		{apierrors.NewServiceUnavailable("etcd"), http.StatusServiceUnavailable},
		{errors.New("connection refused"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		if got := StatusFor(tt.err); got != tt.want {
			t.Errorf("StatusFor(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}
