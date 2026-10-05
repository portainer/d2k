// Package images implements the Docker Engine API image endpoints d2k needs.
//
// d2k stores no images: the kubelet pulls a container's image when the
// container starts, and a failed pull shows in the container's state. These
// endpoints answer what compose and Portainer ask without inventing image
// contents.
//
// Endpoints:
//
//	GET    /images/json        → docker images (empty)
//	POST   /images/create      → docker pull (acknowledged)
//	GET    /images/{name}/json → docker image inspect (the reference only)
//	DELETE /images/{name}      → docker rmi (not supported)
package images

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"

	"github.com/portainer/d2k/internal/apispec"
	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/pkg/httputils"
)

// Handler serves the image endpoints.
type Handler struct{}

// NewHandler creates a Handler.
func NewHandler() *Handler {
	return &Handler{}
}

// DispatchGet routes GET /images/{name}/json to Inspect.
func (h *Handler) DispatchGet(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/json") {
		h.Inspect(w, r)
		return
	}
	apispec.NotRouted(w, r)
}

// List handles GET /images/json. Portainer's dashboard and snapshots list
// images, so the list answers, empty.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	httputils.WriteJSON(w, http.StatusOK, []image.Summary{})
}

// Pull handles POST /images/create (docker pull). Pulling happens on the
// node when a container starts, so the pull is only acknowledged.
func (h *Handler) Pull(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("fromImage")
	if tag := r.URL.Query().Get("tag"); tag != "" {
		sep := ":"
		if strings.HasPrefix(tag, "sha256:") {
			sep = "@"
		}
		ref += sep + tag
	}
	if ref == "" {
		httputils.WriteErrorFrom(w, errdefs.InvalidParameter(errors.New("fromImage is required")))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(jsonstream.Message{
		Status: fmt.Sprintf("%s: not pulled by d2k; the kubelet pulls it when a container starts", ref),
	})
}

// Inspect handles GET /images/{name}/json. Compose inspects each service's
// image before creating containers and pulls on 404, so every reference is
// reported, with an ID derived from the reference and no image config.
func (h *Handler) Inspect(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/images/"), "/json")
	sum := sha256.Sum256([]byte(ref))
	httputils.WriteJSON(w, http.StatusOK, image.InspectResponse{
		ID:          "sha256:" + hex.EncodeToString(sum[:]),
		RepoTags:    []string{ref},
		RepoDigests: []string{},
		Os:          "linux",
		RootFS:      image.RootFS{Type: "layers"}, // the only type Docker reports
	})
}

// Remove handles DELETE /images/{name} (docker rmi).
func (h *Handler) Remove(w http.ResponseWriter, r *http.Request) {
	httputils.WriteErrorFrom(w, errdefs.NotImplemented(errors.New("image removal is not supported by d2k: images are stored on the cluster nodes")))
}
