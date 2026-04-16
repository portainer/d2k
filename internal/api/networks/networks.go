// Package networks implements the Docker Engine API surface for network management.
// Kubernetes namespace networking is flat - all Pods share the same network.
// d2k accepts network calls and returns synthetic responses rather than
// attempting to map Docker network isolation to Kubernetes constructs.
//
// Implemented endpoints:
//
//	GET    /networks               -> docker network ls
//	POST   /networks/create        -> docker network create
//	GET    /networks/{id}          -> docker network inspect
//	DELETE /networks/{id}          -> docker network rm
package networks

import (
	"encoding/json"
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

	// Apply label filters if present. Docker stack rm sends
	// filters={"label":["com.docker.stack.namespace=<stack>"]}
	// Without filtering we return all networks and the CLI deletes them all.
	if rawFilters := r.URL.Query().Get("filters"); rawFilters != "" {
		networks = filterNetworksByLabel(networks, rawFilters)
	}

	httputils.WriteJSON(w, http.StatusOK, networks)
}

func filterNetworksByLabel(networks []adapter.NetworkSummary, rawFilters string) []adapter.NetworkSummary {
	// Docker CLI sends label filters as either:
	//   {"label":["key=value"]}       (array form)
	//   {"label":{"key=value":true}}  (map form, e.g. docker stack rm)
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rawFilters), &parsed); err != nil {
		return networks
	}
	raw, ok := parsed["label"]
	if !ok {
		return networks
	}
	var labelList []string
	if json.Unmarshal(raw, &labelList) != nil {
		var labelMap map[string]bool
		if json.Unmarshal(raw, &labelMap) == nil {
			for k := range labelMap {
				labelList = append(labelList, k)
			}
		}
	}
	if len(labelList) == 0 {
		return networks
	}
	// Parse required labels into key=value pairs.
	required := map[string]string{}
	for _, l := range labelList {
		if idx := strings.Index(l, "="); idx >= 0 {
			required[l[:idx]] = l[idx+1:]
		} else {
			required[l] = ""
		}
	}
	var result []adapter.NetworkSummary
	for _, n := range networks {
		match := true
		for k, v := range required {
			if nv, ok := n.Labels[k]; !ok || (v != "" && nv != v) {
				match = false
				break
			}
		}
		if match {
			result = append(result, n)
		}
	}
	return result
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

	network, err := h.adapter.InspectNetworkDetail(r.Context(), nameOrID)
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