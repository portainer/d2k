// Package networks implements the Docker Engine API surface for network management.
// Kubernetes namespace networking is flat — all Pods share the same network.
// d2k accepts network calls and returns synthetic responses rather than
// attempting to map Docker network isolation to Kubernetes constructs.
//
// Implemented endpoints:
//
//	GET    /networks               → docker network ls
//	POST   /networks/create        → docker network create
//	GET    /networks/{id}          → docker network inspect
//	DELETE /networks/{id}          → docker network rm
package networks

import (
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/pkg/httputils"
)

// Handler holds dependencies for network API endpoints.
type Handler struct {
	adapter *adapter.KubernetesDockerAdapter
	logger  *zap.SugaredLogger
}

// NewHandler creates a Handler.
func NewHandler(a *adapter.KubernetesDockerAdapter, logger *zap.SugaredLogger) *Handler {
	return &Handler{adapter: a, logger: logger}
}

// List handles GET /networks.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	networks, err := h.adapter.ListNetworks(r.Context())
	if err != nil {
		h.logger.Errorw("ListNetworks failed", "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	httputils.WriteJSON(w, http.StatusOK, networks)
}

// createBody mirrors the Docker network create request body.
type createBody struct {
	Name   string            `json:"Name"`
	Driver string            `json:"Driver"`
	Labels map[string]string `json:"Labels"`
}

// Create handles POST /networks/create.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var body createBody
	if err := httputils.ParseJSON(r, &body); err != nil {
		httputils.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	network, warnings, err := h.adapter.CreateNetwork(r.Context(), adapter.CreateNetworkOptions{
		Name:   body.Name,
		Driver: body.Driver,
		Labels: body.Labels,
	})
	if err != nil {
		h.logger.Errorw("CreateNetwork failed", "name", body.Name, "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if len(warnings) > 0 {
		for _, w := range warnings {
			h.logger.Warnw("network warning", "warning", w)
		}
	}

	httputils.WriteJSON(w, http.StatusCreated, map[string]any{
		"Id":      network.ID,
		"Warning": strings.Join(warnings, "; "),
	})
}

// Inspect handles GET /networks/{id}.
func (h *Handler) Inspect(w http.ResponseWriter, r *http.Request) {
	nameOrID := networkIDFromPath(r.URL.Path)

	network, err := h.adapter.InspectNetwork(r.Context(), nameOrID)
	if err != nil {
		h.logger.Errorw("InspectNetwork failed", "id", nameOrID, "error", err)
		httputils.WriteError(w, http.StatusNotFound, err.Error())
		return
	}

	httputils.WriteJSON(w, http.StatusOK, network)
}

// Remove handles DELETE /networks/{id}.
func (h *Handler) Remove(w http.ResponseWriter, r *http.Request) {
	nameOrID := networkIDFromPath(r.URL.Path)

	if err := h.adapter.RemoveNetwork(r.Context(), nameOrID); err != nil {
		h.logger.Errorw("RemoveNetwork failed", "id", nameOrID, "error", err)
		httputils.WriteError(w, http.StatusForbidden, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func networkIDFromPath(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/networks/"), "/")
	return parts[0]
}
