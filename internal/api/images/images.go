// Package images implements the Docker Engine API image endpoints d2k needs.
//
// d2k stores no images: the kubelet pulls a container's image when the
// container starts, and a failed pull shows in the container's state. These
// endpoints answer what compose and Portainer ask without inventing image
// contents.
//
// Endpoints:
//
//	GET    /images/json        → docker images (the images d2k's containers use)
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
	"regexp"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/opencontainers/go-digest"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/internal/apispec"
	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/pkg/httputils"
)

// Handler serves the image endpoints.
type Handler struct {
	adapter *adapter.KubernetesDockerAdapter
}

// NewHandler creates a Handler.
func NewHandler(a *adapter.KubernetesDockerAdapter) *Handler {
	return &Handler{adapter: a}
}

// DispatchGet routes GET /images/{name}/json to Inspect.
func (h *Handler) DispatchGet(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/json") {
		h.Inspect(w, r)
		return
	}
	apispec.NotRouted(w, r)
}

// List handles GET /images/json: the images the containers of d2k's
// namespace use, as the cluster's nodes report them.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	images, err := h.adapter.ListUsedImages(r.Context())
	if err != nil {
		httputils.WriteErrorFrom(w, err)
		return
	}
	httputils.WriteJSON(w, http.StatusOK, images)
}

// Pull handles POST /images/create (docker pull). Pulling happens on the
// node when a container starts, so the pull is only acknowledged.
func (h *Handler) Pull(w http.ResponseWriter, r *http.Request) {
	ref, err := pullReference(r.URL.Query().Get("fromImage"), r.URL.Query().Get("tag"))
	if err != nil {
		httputils.WriteErrorFrom(w, errdefs.InvalidParameter(err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(jsonstream.Message{
		Status: fmt.Sprintf("%s: not pulled by d2k; the kubelet pulls it when a container starts", reference.FamiliarString(ref)),
	})
}

// pullReference combines fromImage and tag as the Docker daemon does
// (moby postImagesCreate): tag replaces a tag in fromImage, and a digest
// in tag replaces any tag or digest.
func pullReference(fromImage, tag string) (reference.Named, error) {
	if fromImage == "" {
		return nil, errors.New("fromImage is required")
	}
	// "docker pull -a" may send the name with a trailing ":".
	ref, err := reference.ParseNormalizedNamed(strings.TrimSuffix(fromImage, ":"))
	if err != nil || tag == "" {
		return ref, err
	}
	if dgst, err := digest.Parse(tag); err == nil {
		return reference.WithDigest(reference.TrimNamed(ref), dgst)
	}
	return reference.WithTag(ref, tag)
}

// Inspect handles GET /images/{name}/json. An image d2k's containers use,
// named by the ID docker images lists, is described as the list does.
// Compose inspects each service's image by name before creating containers
// and pulls on 404, so a name is reported too, with an ID derived from it and
// no image config: the ID stays the same whether a container runs the image
// or not, so compose doesn't take it for a new image and recreate the
// containers.
func (h *Handler) Inspect(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/images/"), "/json")
	if !imageIDPattern.MatchString(ref) {
		h.inspectReference(w, ref)
		return
	}
	if used, err := h.adapter.FindUsedImage(r.Context(), ref); err == nil && used != nil {
		httputils.WriteJSON(w, http.StatusOK, image.InspectResponse{
			ID:          used.ID,
			RepoTags:    used.RepoTags,
			RepoDigests: used.RepoDigests,
			Size:        used.Size,
			Os:          "linux",
			RootFS:      image.RootFS{Type: "layers"},
			Created:     time.Time{}.Format(time.RFC3339Nano),
		})
		return
	}
	h.inspectReference(w, ref)
}

// imageIDPattern matches an image ID, with or without its sha256: prefix.
var imageIDPattern = regexp.MustCompile(`^(sha256:)?[0-9a-f]{64}$`)

// inspectReference reports a reference as an image of its own, with an ID
// derived from it.
func (h *Handler) inspectReference(w http.ResponseWriter, ref string) {
	sum := sha256.Sum256([]byte(ref))
	httputils.WriteJSON(w, http.StatusOK, image.InspectResponse{
		ID:          "sha256:" + hex.EncodeToString(sum[:]),
		RepoTags:    []string{ref},
		RepoDigests: []string{},
		Os:          "linux",
		RootFS:      image.RootFS{Type: "layers"}, // the only type Docker reports
		// d2k does not know when the image was built. Real Docker always sets this
		// to a valid timestamp, which some clients (docker compose v2.40.3's
		// "images" command) parse unconditionally; the zero time is honest (not a
		// made-up build date) and still parses.
		Created: time.Time{}.Format(time.RFC3339Nano),
	})
}

// Remove handles DELETE /images/{name} (docker rmi).
func (h *Handler) Remove(w http.ResponseWriter, r *http.Request) {
	httputils.WriteErrorFrom(w, errdefs.NotImplemented(errors.New("image removal is not supported by d2k: images are stored on the cluster nodes")))
}
