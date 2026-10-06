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
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/moby/moby/api/types/network"
	"github.com/rs/zerolog/log"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/internal/filters"
	"github.com/portainer/d2k/pkg/httputils"
)

// Handler holds dependencies for network API endpoints.
type Handler struct {
	adapter *adapter.KubernetesDockerAdapter
}

// NewHandler creates a Handler.
func NewHandler(a *adapter.KubernetesDockerAdapter) *Handler {
	return &Handler{adapter: a}
}

// List handles GET /networks.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	args, err := httputils.ParseFilters(r, networkFilters)
	if err != nil {
		httputils.WriteErrorFrom(w, err)
		return
	}
	for _, t := range args.Get("type") {
		if t != "builtin" && t != "custom" {
			httputils.WriteErrorFrom(w, errdefs.InvalidParameter(fmt.Errorf("invalid filter: 'type'='%s'", t)))
			return
		}
	}

	networks, err := h.adapter.ListNetworks(r.Context())
	if err != nil {
		log.Error().Str("context", "NetworksAPI").Err(err).Msg("ListNetworks failed")
		httputils.WriteErrorFrom(w, err)
		return
	}

	if values := args.Get("dangling"); len(values) > 0 {
		dangling, err := parseDangling(values)
		if err != nil {
			httputils.WriteErrorFrom(w, err)
			return
		}
		ctrs, err := h.adapter.ListContainers(r.Context(), true)
		if err != nil {
			httputils.WriteErrorFrom(w, err)
			return
		}
		inUse := map[string]bool{}
		for _, c := range ctrs {
			for name := range c.Networks {
				inUse[name] = true
			}
		}
		networks = slices.DeleteFunc(networks, func(n adapter.NetworkSummary) bool {
			return (predefined[n.Name] || inUse[n.Name]) == dangling
		})
	}
	networks = slices.DeleteFunc(networks, func(n adapter.NetworkSummary) bool { return !matches(args, n) })

	httputils.WriteJSON(w, http.StatusOK, networks)
}

// networkFilters are the `filters` keys of GET /networks, as in the daemon.
var networkFilters = map[string]bool{"dangling": true, "driver": true, "id": true, "label": true, "name": true, "scope": true, "type": true}

// predefined are the networks Docker creates itself.
var predefined = map[string]bool{"bridge": true, "host": true, "none": true, "default": true}

// matches applies the network list filters as the daemon does, but dangling.
func matches(args filters.Args, n adapter.NetworkSummary) bool {
	if !args.ExactMatch("driver", n.Driver) || !args.ExactMatch("scope", n.Scope) || !args.MatchKVList("label", n.Labels) {
		return false
	}
	if args.Contains("name") && !args.Match("name", n.Name) {
		return false
	}
	if args.Contains("id") && !args.Match("id", n.ID) {
		return false
	}
	if types := args.Get("type"); len(types) > 0 && (types[0] == "builtin") != predefined[n.Name] {
		return false
	}
	return true
}

func parseDangling(values []string) (bool, error) {
	if len(values) > 1 {
		return false, errdefs.InvalidParameter(errors.New(`got more than one value for filter key "dangling"`))
	}
	switch values[0] {
	case "0", "false":
		return false, nil
	case "1", "true":
		return true, nil
	}
	return false, errdefs.InvalidParameter(errors.New(`invalid value for filter 'dangling', must be "true" (or "1"), or "false" (or "0")`))
}

// Create handles POST /networks/create.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var body network.CreateRequest
	if err := httputils.ParseJSON(r, &body); err != nil {
		httputils.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	network, warnings, err := h.adapter.CreateNetwork(r.Context(), adapter.CreateNetworkOptions{
		Name:           body.Name,
		Driver:         body.Driver,
		Labels:         body.Labels,
		Internal:       body.Internal,
		EnableIPv6:     body.EnableIPv6 != nil && *body.EnableIPv6,
		DisableIPv4:    body.EnableIPv4 != nil && !*body.EnableIPv4,
		IPAMConfigured: body.IPAM != nil && len(body.IPAM.Config) > 0,
		Options:        body.Options,
	})
	if err != nil {
		log.Error().Str("context", "NetworksAPI").Any("name", body.Name).Err(err).Msg("CreateNetwork failed")
		httputils.WriteErrorFrom(w, err)
		return
	}

	if len(warnings) > 0 {
		for _, w := range warnings {
			log.Warn().Str("context", "NetworksAPI").Any("warning", w).Msg("Network warning")
		}
	}

	httputils.WriteJSON(w, http.StatusCreated, map[string]any{
		"Id":      network.ID,
		"Warning": strings.Join(warnings, "; "),
	})
}

// Connect handles POST /networks/{id}/connect and /disconnect. The namespace
// network is flat, so every container already reaches every other; only the
// container's reported network attachments change.
func (h *Handler) Connect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Container string `json:"Container"`
	}
	if err := httputils.ParseJSON(r, &body); err != nil {
		httputils.WriteError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %s", err))
		return
	}
	connect := strings.HasSuffix(r.URL.Path, "/connect")
	if err := h.adapter.ConnectNetwork(r.Context(), r.PathValue("id"), body.Container, connect); err != nil {
		log.Error().Str("context", "NetworksAPI").Any("network", r.PathValue("id")).Any("container", body.Container).Err(err).Msg("ConnectNetwork failed")
		httputils.WriteErrorFrom(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Inspect handles GET /networks/{id}.
func (h *Handler) Inspect(w http.ResponseWriter, r *http.Request) {
	nameOrID := networkIDFromPath(r.URL.Path)

	network, err := h.adapter.InspectNetworkDetail(r.Context(), nameOrID)
	if err != nil {
		log.Error().Str("context", "NetworksAPI").Any("id", nameOrID).Err(err).Msg("InspectNetwork failed")
		httputils.WriteErrorFrom(w, err)
		return
	}

	httputils.WriteJSON(w, http.StatusOK, network)
}

// Remove handles DELETE /networks/{id}.
func (h *Handler) Remove(w http.ResponseWriter, r *http.Request) {
	nameOrID := networkIDFromPath(r.URL.Path)

	if err := h.adapter.RemoveNetwork(r.Context(), nameOrID); err != nil {
		log.Error().Str("context", "NetworksAPI").Any("id", nameOrID).Err(err).Msg("RemoveNetwork failed")
		httputils.WriteErrorFrom(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func networkIDFromPath(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/networks/"), "/")
	return parts[0]
}
