// Package router builds and returns the http.ServeMux that implements the
// Docker Engine API surface exposed by d2k.
package router

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/internal/api/containers"
	"github.com/portainer/d2k/internal/api/events"
	"github.com/portainer/d2k/internal/api/exec"
	"github.com/portainer/d2k/internal/api/images"
	"github.com/portainer/d2k/internal/api/networks"
	"github.com/portainer/d2k/internal/api/swarm"
	"github.com/portainer/d2k/internal/api/system"
	"github.com/portainer/d2k/internal/api/volumes"
	"github.com/portainer/d2k/internal/apispec"
	"github.com/portainer/d2k/internal/middleware"
	"github.com/portainer/d2k/pkg/httputils"
)

// New builds the router with all Docker API endpoints registered.
// If swarmMode is true, the Docker Swarm API surface is also registered.
func New(a *adapter.KubernetesDockerAdapter, namespace string, swarmMode bool) http.Handler {
	mux := http.NewServeMux()

	sys := system.NewHandler(a, namespace, swarmMode)
	c := containers.NewHandler(a)
	v := volumes.NewHandler(a)
	n := networks.NewHandler(a)
	img := images.NewHandler(a)
	ev := events.NewHandler(a)
	e := exec.NewHandler(a)

	// System
	mux.HandleFunc("GET /_ping", sys.Ping)
	mux.HandleFunc("HEAD /_ping", sys.Ping)
	mux.HandleFunc("GET /version", sys.Version)
	mux.HandleFunc("GET /info", sys.Info)

	// Containers
	mux.HandleFunc("GET /containers/json", c.List)
	mux.HandleFunc("POST /containers/create", c.Create)
	mux.HandleFunc("POST /containers/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/exec") {
			e.Create(w, r)
			return
		}
		c.DispatchAction(w, r)
	})
	mux.HandleFunc("POST /exec/{id}/start", e.Start)
	mux.HandleFunc("POST /exec/{id}/resize", e.Resize)
	mux.HandleFunc("GET /exec/{id}/json", e.Inspect)
	mux.HandleFunc("DELETE /containers/", c.Remove)
	mux.HandleFunc("PUT /containers/{id}/archive", c.PutArchive)
	mux.HandleFunc("GET /containers/", c.DispatchGet) // /containers/{id}/json|logs

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
	mux.HandleFunc("POST /networks/{id}/connect", n.Connect)
	mux.HandleFunc("POST /networks/{id}/disconnect", n.Connect)
	mux.HandleFunc("GET /networks/", n.Inspect)
	mux.HandleFunc("DELETE /networks/", n.Remove)

	// Events
	mux.HandleFunc("GET /events", ev.Stream)

	// Swarm mode - only registered when D2K_SWARM_MODE=true.
	if swarmMode {
		log.Info().Str("context", "Router").Msg("Swarm mode enabled - registering Swarm API endpoints")
		sw := swarm.NewHandler(a, namespace)

		mux.HandleFunc("GET /swarm", sw.InspectSwarm)
		mux.HandleFunc("POST /swarm/init", sw.InitSwarm)
		mux.HandleFunc("POST /swarm/leave", sw.LeaveSwarm)

		mux.HandleFunc("GET /nodes", sw.ListNodes)
		mux.HandleFunc("GET /nodes/", sw.DispatchNode)
		mux.HandleFunc("POST /nodes/", sw.DispatchNode)

		mux.HandleFunc("POST /services/create", sw.CreateService)
		mux.HandleFunc("GET /services", sw.ListServices)
		mux.HandleFunc("GET /services/", sw.DispatchService)
		mux.HandleFunc("POST /services/", sw.DispatchService)
		mux.HandleFunc("DELETE /services/", sw.DispatchService)

		mux.HandleFunc("GET /tasks", sw.ListTasks)
		mux.HandleFunc("GET /tasks/", sw.InspectTask)

		mux.HandleFunc("POST /secrets/create", sw.CreateSecret)
		mux.HandleFunc("GET /secrets", sw.ListSecrets)
		mux.HandleFunc("GET /secrets/", sw.DispatchSecret)
		mux.HandleFunc("POST /secrets/", sw.DispatchSecret)
		mux.HandleFunc("DELETE /secrets/", sw.DispatchSecret)

		mux.HandleFunc("POST /configs/create", sw.CreateConfig)
		mux.HandleFunc("GET /configs", sw.ListConfigs)
		mux.HandleFunc("GET /configs/", sw.DispatchConfig)
		mux.HandleFunc("POST /configs/", sw.DispatchConfig)
		mux.HandleFunc("DELETE /configs/", sw.DispatchConfig)

		mux.HandleFunc("GET /distribution/", sw.DistributionInspect)
	}

	// Catch-all for unmatched routes - logs the method and path for debugging.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Warn().Str("context", "Router").Any("method", r.Method).Any("path", r.URL.Path).Msg("Unmatched route")
		apispec.NotRouted(w, r)
	})

	// Versioned path prefix stripping - Docker CLI sends /v1.52/containers/json etc.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path := stripAPIVersion(r.URL.Path); path != r.URL.Path {
			if err := apispec.CheckVersion(r.URL.Path[2 : len(r.URL.Path)-len(path)]); err != nil {
				httputils.WriteErrorFrom(w, err)
				return
			}
			r2 := r.Clone(r.Context())
			// Clone the URL to avoid mutating the original request's URL,
			// which would corrupt logging and any subsequent middleware reads.
			urlCopy := *r.URL
			urlCopy.Path = path
			r2.URL = &urlCopy
			mux.ServeHTTP(w, r2)
			return
		}
		mux.ServeHTTP(w, r)
	})

	return middleware.Logging(middleware.RequestID(handler))
}

// apiVersionPrefix matches the API version prefix Docker clients send, e.g. "/v1.52".
var apiVersionPrefix = regexp.MustCompile(`^/v[0-9]+\.[0-9]+/`)

// stripAPIVersion removes a leading /v<major>.<minor> segment. Unversioned
// paths such as /volumes/create are returned unchanged.
func stripAPIVersion(path string) string {
	if loc := apiVersionPrefix.FindStringIndex(path); loc != nil {
		return path[loc[1]-1:]
	}
	return path
}
