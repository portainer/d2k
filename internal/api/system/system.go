// Package system implements the Docker Engine system endpoints.
// These are required by every Docker client as handshake/capability discovery.
//
// Implemented endpoints:
//
//	GET  /_ping     -> liveness check, returns "OK"
//	GET  /version   -> Docker API version negotiation
//	GET  /info      -> cluster/daemon info
package system

import (
	"context"
	"net/http"
	"runtime"

	"github.com/rs/zerolog/log"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/internal/types"
	"github.com/portainer/d2k/pkg/httputils"
)

// Handler holds dependencies for system endpoints.
type Handler struct {
	adapter   *adapter.KubernetesDockerAdapter
	namespace string
	swarmMode bool
}

// NewHandler creates a Handler.
func NewHandler(a *adapter.KubernetesDockerAdapter, namespace string, swarmMode bool) *Handler {
	return &Handler{adapter: a, namespace: namespace, swarmMode: swarmMode}
}

// Ping handles GET /_ping.
func (h *Handler) Ping(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("API-Version", types.DockerAPIVersion)
	w.Header().Set("Docker-Experimental", "false")
	w.Header().Set("OSType", "linux")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// Version handles GET /version.
func (h *Handler) Version(w http.ResponseWriter, r *http.Request) {
	details := map[string]string{
		"ApiVersion":    types.DockerAPIVersion,
		"MinAPIVersion": "1.24",
		"Os":            runtime.GOOS,
		"Arch":          runtime.GOARCH,
		"GoVersion":     runtime.Version(),
		"KernelVersion": "d2k",
	}
	body := map[string]any{
		"Version": types.Version,
		// Recent Docker CLIs read the server details from its components.
		"Components": []map[string]any{{"Name": "Engine", "Version": types.Version, "Details": details}},
	}
	for k, v := range details {
		body[k] = v
	}
	httputils.WriteJSON(w, http.StatusOK, body)
}

// swarmInfo returns the Swarm section of the /info response.
// When swarm mode is active, Portainer (and Docker CLI) use LocalNodeState and
// ControlAvailable to decide whether to show the Swarm UI.
// Node counts and the manager node ID are derived from the live Kubernetes cluster
// so multi-node clusters are represented correctly.
func (h *Handler) swarmInfo(ctx context.Context) map[string]any {
	if !h.swarmMode {
		return map[string]any{
			"LocalNodeState": "inactive",
		}
	}

	// Fetch live node list to get accurate counts.
	nodes, err := h.adapter.SwarmListNodes(ctx)
	if err != nil {
		log.Warn().Str("context", "SystemAPI").Err(err).Msg("SwarmInfo: unable to list nodes")
	}
	totalNodes := len(nodes)
	managerCount := 0
	for _, n := range nodes {
		if spec, ok := n["Spec"].(map[string]any); ok {
			if spec["Role"] == "manager" {
				managerCount++
			}
		}
	}
	if totalNodes == 0 {
		totalNodes = 1
	}
	if managerCount == 0 {
		managerCount = 1
	}

	// Fetch stable cluster identity so NodeID matches what /swarm returns.
	nodeID := "d2k"
	clusterID := "d2k-cluster"
	identity, err := h.adapter.SwarmIdentity(ctx)
	if err != nil {
		log.Warn().Str("context", "SystemAPI").Err(err).Msg("SwarmInfo: unable to get swarm identity")
	} else {
		if id, ok := identity["NodeID"].(string); ok && id != "" {
			nodeID = id
		}
		if id, ok := identity["ID"].(string); ok && id != "" {
			clusterID = id
		}
	}

	return map[string]any{
		"LocalNodeState":   "active",
		"ControlAvailable": true,
		"Error":            "",
		"NodeID":           nodeID,
		"NodeAddr":         "",
		"RemoteManagers":   []map[string]any{{"NodeID": nodeID, "Addr": ""}},
		"Nodes":            totalNodes,
		"Managers":         managerCount,
		"Cluster": map[string]any{
			"ID":      clusterID,
			"Version": map[string]any{"Index": uint64(1)},
			"Spec": map[string]any{
				"Name":   "d2k",
				"Labels": map[string]string{},
				"Orchestration": map[string]any{
					"TaskHistoryRetentionLimit": 5,
				},
			},
		},
	}
}

// Info handles GET /info.
func (h *Handler) Info(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Docker clients call /info constantly; report what can be read rather
	// than fail it.
	id, err := h.adapter.HostID(ctx)
	if err != nil {
		log.Warn().Str("context", "SystemAPI").Err(err).Msg("Info: unable to read the host ID")
		id = h.namespace
	}
	ncpu, memory, err := h.adapter.HostResources(ctx)
	if err != nil {
		log.Warn().Str("context", "SystemAPI").Err(err).Msg("Info: unable to read host resources")
	}
	ctrs, err := h.adapter.ListContainers(ctx, true)
	if err != nil {
		log.Warn().Str("context", "SystemAPI").Err(err).Msg("Info: unable to count containers")
	}
	running := 0
	for _, c := range ctrs {
		if c.State == "running" {
			running++
		}
	}

	httputils.WriteJSON(w, http.StatusOK, map[string]any{
		"ID":                id,
		"Containers":        len(ctrs),
		"ContainersRunning": running,
		"ContainersPaused":  0,
		"ContainersStopped": len(ctrs) - running,
		"Name":              "Portainer d2k translator",
		"ServerVersion":     types.Version,
		"OperatingSystem":   "Kubernetes/" + h.namespace,
		"OSType":            "linux",
		"Architecture":      runtime.GOARCH,
		"DockerRootDir":     "/var/lib/d2k",
		"HttpProxy":         "",
		"HttpsProxy":        "",
		"NoProxy":           "",
		"ExperimentalBuild": false,
		"LoggingDriver":     "json-file",
		"CgroupDriver":      "cgroupfs",
		"CgroupVersion":     "2",
		"MemoryLimit":       true,
		"SwapLimit":         true,
		"CPUCfsPeriod":      true,
		"CPUCfsQuota":       true,
		"CPUShares":         true,
		"CPUSet":            true,
		"IPv4Forwarding":    true,
		"BridgeNfIptables":  true,
		"BridgeNfIp6tables": true,
		"OomKillDisable":    true,
		"NGoroutines":       10,
		"NCPU":              ncpu,
		"MemTotal":          memory,
		"SecurityOptions":   []string{},
		"Swarm":             h.swarmInfo(ctx),
		"Labels": []string{
			"d2k.portainer.io/translator=true",
			"d2k.portainer.io/namespace=" + h.namespace,
		},
		"Plugins": map[string]any{
			"Volume":        []string{},
			"Network":       []string{"bridge", "host", "null"},
			"Log":           []string{"json-file"},
			"Authorization": nil,
			"Builder":       nil,
		},
	})
}
