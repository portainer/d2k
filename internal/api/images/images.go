// Package images implements a minimal Docker Engine API surface for image operations.
//
// In a Kubernetes environment, image management is handled by the container runtime
// on each node. Pull operations are accepted and acknowledged (Kubernetes will pull
// on schedule), and the image list is synthesised from images referenced by
// d2k-managed Deployments.
//
// Endpoints:
//
//	GET    /images/json        → docker images
//	POST   /images/create      → docker pull (acknowledged, no-op)
//	GET    /images/{name}/json → docker image inspect (synthesised)
//	DELETE /images/{name}      → docker rmi (not supported)
package images

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/pkg/httputils"
)

// Handler holds dependencies for image API endpoints.
type Handler struct {
	adapter *adapter.KubernetesDockerAdapter
	logger  *zap.SugaredLogger
}

// NewHandler creates a Handler.
func NewHandler(a *adapter.KubernetesDockerAdapter, logger *zap.SugaredLogger) *Handler {
	return &Handler{adapter: a, logger: logger}
}

// DispatchGet routes GET /images/{name}/json to Inspect.
func (h *Handler) DispatchGet(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/json") {
		h.Inspect(w, r)
		return
	}
	http.NotFound(w, r)
}

// List handles GET /images/json (docker images).
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	ctrs, err := h.adapter.ListContainers(r.Context(), true)
	if err != nil {
		h.logger.Errorw("ListContainers for image list failed", "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type imageSummary struct {
		ID          string   `json:"Id"`
		RepoTags    []string `json:"RepoTags"`
		Created     int64    `json:"Created"`
		Size        int64    `json:"Size"`
		VirtualSize int64    `json:"VirtualSize"`
	}

	seen := map[string]bool{}
	items := []imageSummary{}
	for _, c := range ctrs {
		if seen[c.Image] {
			continue
		}
		seen[c.Image] = true
		items = append(items, imageSummary{
			ID:       fmt.Sprintf("sha256:d2k%x", []byte(c.Image)),
			RepoTags: []string{c.Image},
			Created:  time.Now().Unix(),
		})
	}

	httputils.WriteJSON(w, http.StatusOK, items)
}

// Pull handles POST /images/create (docker pull).
// Returns a synthetic progress stream — Kubernetes will pull at schedule time.
func (h *Handler) Pull(w http.ResponseWriter, r *http.Request) {
	fromImage := r.URL.Query().Get("fromImage")
	tag := r.URL.Query().Get("tag")
	if tag == "" {
		tag = "latest"
	}
	ref := fromImage
	if !strings.Contains(fromImage, ":") {
		ref = fmt.Sprintf("%s:%s", fromImage, tag)
	}

	h.logger.Infow("docker pull acknowledged — Kubernetes pulls at schedule time", "image", ref)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	events := []string{
		fmt.Sprintf(`{"status":"Pulling from %s","id":"%s"}`, fromImage, tag),
		`{"status":"Pull complete","progressDetail":{}}`,
		`{"status":"Digest: sha256:d2k-noop"}`,
		fmt.Sprintf(`{"status":"Status: Image will be pulled by Kubernetes when scheduled: %s"}`, ref),
	}
	for _, e := range events {
		fmt.Fprintln(w, e)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// Inspect handles GET /images/{name}/json (docker image inspect).
func (h *Handler) Inspect(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/images/"), "/json")
	httputils.WriteJSON(w, http.StatusOK, map[string]any{
		"Id":       fmt.Sprintf("sha256:d2k%x", []byte(name)),
		"RepoTags": []string{name},
		"Created":  time.Now().Format(time.RFC3339),
		"Comment":  "synthesised by d2k — actual image metadata lives on cluster nodes",
		"Config":   map[string]any{"Image": name},
	})
}

// Remove handles DELETE /images/{name} (docker rmi). Not supported.
func (h *Handler) Remove(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/images/")
	httputils.WriteError(w, http.StatusMethodNotAllowed,
		fmt.Sprintf("image removal is not supported by d2k: %q lives on cluster nodes", name),
	)
}
