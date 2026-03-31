// Package router builds and returns the http.ServeMux that implements the
// Docker Engine API surface exposed by d2k.
package router

import (
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/internal/api/containers"
	"github.com/portainer/d2k/internal/api/images"
	"github.com/portainer/d2k/internal/api/networks"
	"github.com/portainer/d2k/internal/api/system"
	"github.com/portainer/d2k/internal/api/volumes"
	"github.com/portainer/d2k/internal/middleware"
	"github.com/portainer/d2k/internal/api/exec"
)

// New builds the router with all Docker API endpoints registered.
func New(a *adapter.KubernetesDockerAdapter, namespace string, logger *zap.SugaredLogger) http.Handler {
	mux := http.NewServeMux()

	sys := system.NewHandler(namespace, logger)
	c := containers.NewHandler(a, logger)
	e := exec.NewHandler(a, logger)
	v := volumes.NewHandler(a, logger)
	n := networks.NewHandler(a, logger)
	img := images.NewHandler(a, logger)

	// System
	mux.HandleFunc("GET /_ping", sys.Ping)
	mux.HandleFunc("HEAD /_ping", sys.Ping)
	mux.HandleFunc("GET /version", sys.Version)
	mux.HandleFunc("GET /info", sys.Info)

	// Containers
	mux.HandleFunc("GET /containers/json", c.List)
	mux.HandleFunc("POST /containers/create", c.Create)
	mux.HandleFunc("POST /containers/", c.DispatchAction) // /containers/{id}/start|stop
	mux.HandleFunc("DELETE /containers/", c.Remove)
	mux.HandleFunc("GET /containers/", c.DispatchGet) // /containers/{id}/json|logs

	// Exec
	mux.HandleFunc("POST /containers/", c.DispatchAction) // already exists, but also catches /exec
	mux.HandleFunc("POST /exec/", e.Start)
	mux.HandleFunc("GET /exec/", e.Inspect)

	// Images
	mux.HandleFunc("GET /images/json", img.List)
	mux.HandleFunc("POST /images/create", img.Pull)
	mux.HandleFunc("GET /images/", img.DispatchGet)
	mux.HandleFunc("DELETE /images/", img.Remove)

	// Volumes
	mux.HandleFunc("GET /volumes", v.List)
	mux.HandleFunc("POST /volumes/create", v.Create)
	mux.HandleFunc("GET /volumes/", v.Inspect)
	mux.HandleFunc("DELETE /volumes/", v.Remove)

	// Networks
	mux.HandleFunc("GET /networks", n.List)
	mux.HandleFunc("POST /networks/create", n.Create)
	mux.HandleFunc("GET /networks/", n.Inspect)
	mux.HandleFunc("DELETE /networks/", n.Remove)

	// Versioned path prefix stripping — Docker CLI sends /v1.41/containers/json etc.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v") {
			// Find the end of the version segment, e.g. /v1.41/...
			rest := r.URL.Path[1:] // strip leading /
			if idx := strings.Index(rest, "/"); idx != -1 {
				r2 := r.Clone(r.Context())
				r2.URL = r.URL
				r2.URL.Path = rest[idx:] // /containers/json etc.
				mux.ServeHTTP(w, r2)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})

	return middleware.Logging(logger)(middleware.RequestID(handler))
}
