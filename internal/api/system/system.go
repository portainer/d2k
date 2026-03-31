// Package system implements the Docker Engine system endpoints.
// These are required by every Docker client as handshake/capability discovery.
//
// Implemented endpoints:
//
//	GET  /_ping     → liveness check, returns "OK"
//	GET  /version   → Docker API version negotiation
//	GET  /info      → cluster/daemon info
package system

import (
	"net/http"
	"runtime"

	"go.uber.org/zap"

	"github.com/portainer/d2k/internal/types"
	"github.com/portainer/d2k/pkg/httputils"
)

// Handler holds dependencies for system endpoints.
type Handler struct {
	namespace string
	logger    *zap.SugaredLogger
}

// NewHandler creates a Handler.
func NewHandler(namespace string, logger *zap.SugaredLogger) *Handler {
	return &Handler{namespace: namespace, logger: logger}
}

// Ping handles GET /_ping.
func (h *Handler) Ping(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("API-Version", types.DockerAPIVersion)
	w.Header().Set("Docker-Experimental", "false")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// Version handles GET /version.
func (h *Handler) Version(w http.ResponseWriter, r *http.Request) {
	httputils.WriteJSON(w, http.StatusOK, map[string]any{
		"Version":       types.Version,
		"ApiVersion":    types.DockerAPIVersion,
		"MinAPIVersion": "1.24",
		"Os":            runtime.GOOS,
		"Arch":          runtime.GOARCH,
		"KernelVersion": "d2k",
	})
}

// Info handles GET /info.
func (h *Handler) Info(w http.ResponseWriter, r *http.Request) {
	httputils.WriteJSON(w, http.StatusOK, map[string]any{
    "ID":                "d2k",
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
    "NCPU":              1,
    "MemTotal":          int64(2 * 1024 * 1024 * 1024),
    "SecurityOptions":   []string{},
    "Swarm": map[string]any{
        "LocalNodeState": "inactive",
    },
    "Labels": []string{
        "d2k.portainer.io/translator=true",
        "d2k.portainer.io/namespace=" + h.namespace,
    },
    "Plugins": map[string]any{
        "Volume":  []string{},
        "Network": []string{"bridge", "host", "null"},
        "Log":     []string{},
    },
})
}
