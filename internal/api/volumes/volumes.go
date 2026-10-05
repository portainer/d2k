// Package volumes implements the Docker Engine API surface for volume management,
// translating each endpoint into Kubernetes PVC operations.
//
// Implemented endpoints:
//
//	GET    /volumes                → docker volume ls
//	POST   /volumes/create        → docker volume create
//	GET    /volumes/{name}        → docker volume inspect
//	DELETE /volumes/{name}        → docker volume rm
package volumes

import (
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/docker/docker/api/types/filters"
	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/pkg/httputils"
)

// Handler holds dependencies for volume API endpoints.
type Handler struct {
	adapter *adapter.KubernetesDockerAdapter
	logger  *zap.SugaredLogger
}

// NewHandler creates a Handler.
func NewHandler(a *adapter.KubernetesDockerAdapter, logger *zap.SugaredLogger) *Handler {
	return &Handler{adapter: a, logger: logger}
}

// List handles GET /volumes.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	args, err := httputils.ParseFilters(r, volumeFilters)
	if err != nil {
		httputils.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	vols, err := h.adapter.ListVolumes(r.Context())
	if err != nil {
		h.logger.Errorw("ListVolumes failed", "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	vols = filterVolumes(vols, args)

	// Docker wraps the list in a Volumes key.
	httputils.WriteJSON(w, http.StatusOK, map[string]any{
		"Volumes":  vols,
		"Warnings": []string{},
	})
}

// volumeFilters are the `filters` keys d2k supports on GET /volumes.
var volumeFilters = map[string]bool{"name": true, "label": true, "driver": true}

// filterVolumes applies list filters with the Docker daemon's matching rules.
func filterVolumes(vols []adapter.VolumeSummary, args filters.Args) []adapter.VolumeSummary {
	result := []adapter.VolumeSummary{}
	for _, v := range vols {
		if args.Match("name", v.Name) && args.ExactMatch("driver", v.Driver) && args.MatchKVList("label", v.Labels) {
			result = append(result, v)
		}
	}
	return result
}

// createBody mirrors the Docker volume create request body.
type createBody struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Labels     map[string]string `json:"Labels"`
	DriverOpts map[string]string `json:"DriverOpts"`
}

// Create handles POST /volumes/create.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var body createBody
	if err := httputils.ParseJSON(r, &body); err != nil {
		httputils.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Allow size to be passed as a driver option: --opt size=5Gi
	size := ""
	if body.DriverOpts != nil {
		size = body.DriverOpts["size"]
	}

	vol, err := h.adapter.CreateVolume(r.Context(), adapter.CreateVolumeOptions{
		Name:       body.Name,
		Driver:     body.Driver,
		Labels:     body.Labels,
		SizeLimit:  size,
		DriverOpts: body.DriverOpts,
	})
	if err != nil {
		h.logger.Errorw("CreateVolume failed", "name", body.Name, "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	httputils.WriteJSON(w, http.StatusCreated, vol)
}

// Inspect handles GET /volumes/{name}.
func (h *Handler) Inspect(w http.ResponseWriter, r *http.Request) {
	name := volumeNameFromPath(r.URL.Path)

	vol, err := h.adapter.InspectVolume(r.Context(), name)
	if err != nil {
		h.logger.Debugw("InspectVolume not found", "name", name, "error", err)
		httputils.WriteError(w, http.StatusNotFound, err.Error())
		return
	}

	httputils.WriteJSON(w, http.StatusOK, vol)
}

// Remove handles DELETE /volumes/{name}.
func (h *Handler) Remove(w http.ResponseWriter, r *http.Request) {
	name := volumeNameFromPath(r.URL.Path)

	if err := h.adapter.RemoveVolume(r.Context(), name); err != nil {
		h.logger.Errorw("RemoveVolume failed", "name", name, "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func volumeNameFromPath(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/volumes/"), "/")
	return parts[0]
}
