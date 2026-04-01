// Package containers implements the Docker Engine API surface for container
// lifecycle management, translating each endpoint into Kubernetes Deployment
// operations via the adapter layer.
//
// Endpoints handled:
//
//	GET    /containers/json           → docker ps / docker ps -a
//	POST   /containers/create         → docker run (create phase)
//	POST   /containers/{id}/start     → docker start
//	POST   /containers/{id}/stop      → docker stop
//	DELETE /containers/{id}           → docker rm
//	GET    /containers/{id}/json      → docker inspect
//	GET    /containers/{id}/logs      → docker logs
package containers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/docker/docker/pkg/namesgenerator"
	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/pkg/httputils"
)

// Handler holds dependencies for all container API endpoints.
type Handler struct {
	adapter *adapter.KubernetesDockerAdapter
	logger  *zap.SugaredLogger
}

// NewHandler creates a Handler.
func NewHandler(a *adapter.KubernetesDockerAdapter, logger *zap.SugaredLogger) *Handler {
	return &Handler{adapter: a, logger: logger}
}

// DispatchAction handles POST /containers/{id}/start, /stop, /restart, /wait.
func (h *Handler) DispatchAction(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/start"):
		h.Start(w, r)
	case strings.HasSuffix(path, "/stop"):
		h.Stop(w, r)
	case strings.HasSuffix(path, "/restart"):
		h.Restart(w, r)
	case strings.HasSuffix(path, "/wait"):
		h.Wait(w, r)
	case strings.HasSuffix(path, "/attach"):
		h.Attach(w, r)
	case strings.HasSuffix(path, "/rename"):
    h.Rename(w, r)
	default:
		http.NotFound(w, r)
	}
}

// Rename handles POST /containers/{id}/rename.
// d2k ignores rename requests — container names are Kubernetes Deployment names
// and cannot be renamed without recreating the Deployment.
func (h *Handler) Rename(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// DispatchGet handles GET /containers/{id}/json and GET /containers/{id}/logs.
func (h *Handler) DispatchGet(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/json"):
		h.Inspect(w, r)
	case strings.HasSuffix(path, "/logs"):
		h.Logs(w, r)
	case strings.HasSuffix(path, "/stats"):
		h.Stats(w, r)
	default:
		http.NotFound(w, r)
	}
}

// List handles GET /containers/json (docker ps / docker ps -a).
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"

	ctrs, err := h.adapter.ListContainers(r.Context(), all)
	if err != nil {
		h.logger.Errorw("ListContainers failed", "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type portBinding struct {
		IP          string `json:"IP"`
		PrivatePort uint16 `json:"PrivatePort"`
		PublicPort  uint16 `json:"PublicPort"`
		Type        string `json:"Type"`
	}

	type item struct {
		ID        string            `json:"Id"`
		Names     []string          `json:"Names"`
		Image     string            `json:"Image"`
		Status    string            `json:"Status"`
		State     string            `json:"State"`
		Created   int64             `json:"Created"`
		Ports     []portBinding     `json:"Ports"`
		Labels    map[string]string `json:"Labels"`
		IPAddress string            `json:"IPAddress"`
	}

	result := make([]item, 0, len(ctrs))
	for _, c := range ctrs {
		var ports []portBinding
		for _, p := range c.Ports {
			ports = append(ports, portBinding{
				IP:          p.IP,
				PrivatePort: p.PrivatePort,
				PublicPort:  p.PublicPort,
				Type:        p.Type,
			})
		}
		result = append(result, item{
			ID:        c.ID,
			Names:     c.Names,
			Image:     c.Image,
			Status:    c.Status,
			State:     c.State,
			Created:   c.Created,
			Labels:    c.Labels,
			IPAddress: c.IPAddress,
			Ports:     ports,
		})
	}

	httputils.WriteJSON(w, http.StatusOK, result)
}

// createBody mirrors the subset of the Docker create request body that d2k uses.
type createBody struct {
	Image        string              `json:"Image"`
	Cmd          []string            `json:"Cmd"`
	Env          []string            `json:"Env"`
	Labels       map[string]string   `json:"Labels"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	HostConfig   struct {
		PortBindings map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"PortBindings"`
		PublishAllPorts bool `json:"PublishAllPorts"`
	} `json:"HostConfig"`
}

// Create handles POST /containers/create (docker run — create phase).
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = strings.ReplaceAll(namesgenerator.GetRandomName(0), "_", "-")
	}

	var body createBody
	if err := httputils.ParseJSON(r, &body); err != nil {
		httputils.WriteError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %s", err))
		return
	}

	// Translate Docker HostConfig.PortBindings map into raw "-p" strings.
	var portBindings []string
	for containerPortProto, hostBindings := range body.HostConfig.PortBindings {
		containerPort := strings.SplitN(containerPortProto, "/", 2)[0]
		for _, hb := range hostBindings {
			if hb.HostIP != "" && hb.HostIP != "0.0.0.0" {
				portBindings = append(portBindings, fmt.Sprintf("%s:%s:%s", hb.HostIP, hb.HostPort, containerPort))
			} else {
				portBindings = append(portBindings, fmt.Sprintf("%s:%s", hb.HostPort, containerPort))
			}
		}
	}

	opts := adapter.RunOptions{
		Name:         name,
		Image:        body.Image,
		Cmd:          body.Cmd,
		Env:          body.Env,
		Labels:       body.Labels,
		ExposedPorts: body.ExposedPorts,
		PortBindings: portBindings,
		PublishAll:   body.HostConfig.PublishAllPorts,
	}

	id, warnings, err := h.adapter.CreateContainer(r.Context(), opts)
	if err != nil {
		h.logger.Errorw("CreateContainer failed", "name", name, "error", err)
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "already in use") {
			status = http.StatusConflict
		}
		httputils.WriteError(w, status, err.Error())
		return
	}

	httputils.WriteJSON(w, http.StatusCreated, map[string]any{
		"Id":       id,
		"Warnings": warnings,
	})
}

// Wait handles POST /containers/{id}/wait.
func (h *Handler) Wait(w http.ResponseWriter, r *http.Request) {
	httputils.WriteJSON(w, http.StatusOK, map[string]any{
		"StatusCode": 0,
		"Error":      nil,
	})
}

// Start handles POST /containers/{id}/start.
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "/start")
	if err := h.adapter.StartContainer(r.Context(), name); err != nil {
		h.logger.Errorw("StartContainer failed", "name", name, "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Stop handles POST /containers/{id}/stop.
func (h *Handler) Stop(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "/stop")
	if err := h.adapter.StopContainer(r.Context(), name); err != nil {
		h.logger.Errorw("StopContainer failed", "name", name, "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Restart handles POST /containers/{id}/restart.
func (h *Handler) Restart(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "/restart")
	if err := h.adapter.StopContainer(r.Context(), name); err != nil {
		h.logger.Errorw("Restart/stop failed", "name", name, "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.adapter.StartContainer(r.Context(), name); err != nil {
		h.logger.Errorw("Restart/start failed", "name", name, "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Remove handles DELETE /containers/{id}.
func (h *Handler) Remove(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "")
	if err := h.adapter.RemoveContainer(r.Context(), name); err != nil {
		h.logger.Errorw("RemoveContainer failed", "name", name, "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Inspect handles GET /containers/{id}/json.
func (h *Handler) Inspect(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "/json")
	result, err := h.adapter.InspectContainer(r.Context(), name)
	if err != nil {
		h.logger.Errorw("InspectContainer failed", "name", name, "error", err)
		httputils.WriteError(w, http.StatusNotFound, err.Error())
		return
	}
	httputils.WriteJSON(w, http.StatusOK, result)
}

// Logs handles GET /containers/{id}/logs.
func (h *Handler) Logs(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "/logs")
	q := r.URL.Query()

	opts := adapter.LogOptions{
		Follow:     q.Get("follow") == "1" || q.Get("follow") == "true",
		Timestamps: q.Get("timestamps") == "1" || q.Get("timestamps") == "true",
		Tail:       q.Get("tail"),
	}

	logs, err := h.adapter.GetContainerLogs(r.Context(), name, opts)
	if err != nil {
		h.logger.Errorw("GetContainerLogs failed", "name", name, "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer logs.Close()

	w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
	if opts.Follow {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
	}

	buf := make([]byte, 32*1024)
	for {
		n, err := logs.Read(buf)
		if n > 0 {
			header := []byte{
				1, // stdout
				0, 0, 0,
				byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n),
			}
			w.Write(header)
			w.Write(buf[:n])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err != nil {
			break
		}
	}
}

// containerName extracts the container name from a path like /containers/<name>/start
// by stripping the /containers/ prefix and the given action suffix.
func containerName(path, suffix string) string {
	s := strings.TrimPrefix(path, "/containers/")
	s = strings.TrimSuffix(s, suffix)
	if idx := strings.Index(s, "/"); idx != -1 {
		s = s[:idx]
	}
	return s
}

// Stats handles GET /containers/{id}/stats.
func (h *Handler) Stats(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "/stats")
	stream := r.URL.Query().Get("stream") != "false"

	resolved, err := h.adapter.ResolveContainerName(r.Context(), name)
	if err != nil {
		httputils.WriteError(w, http.StatusNotFound, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	for {
		metrics, err := h.adapter.GetPodMetrics(r.Context(), resolved)
		if err != nil {
			return
		}

		now := time.Now().UTC().Format(time.RFC3339Nano)
		stats := map[string]any{
			"read":    now,
			"preread": now,
			"cpu_stats": map[string]any{
				"cpu_usage": map[string]any{
					"total_usage":         metrics.CPUUsageNanoCores,
					"usage_in_kernelmode": int64(0),
					"usage_in_usermode":   metrics.CPUUsageNanoCores,
				},
				"system_cpu_usage": int64(0),
				"num_cpus":         1,
				"throttling_data": map[string]any{
					"throttled_periods": 0,
					"throttled_time":    0,
				},
			},
			"precpu_stats": map[string]any{
				"cpu_usage": map[string]any{
					"total_usage":         metrics.PrevCPUUsageNanoCores,
					"usage_in_kernelmode": int64(0),
					"usage_in_usermode":   metrics.PrevCPUUsageNanoCores,
				},
				"system_cpu_usage": int64(0),
				"throttling_data": map[string]any{
					"throttled_periods": 0,
					"throttled_time":    0,
				},
			},
			"memory_stats": map[string]any{
				"usage":    metrics.MemoryUsageBytes,
				"maxusage": metrics.MemoryUsageBytes,
				"limit":    int64(1<<63 - 1),
				"stats":    map[string]any{},
			},
			"networks": map[string]any{},
			"blkio_stats": map[string]any{
				"io_service_bytes_recursive": []any{},
				"io_serviced_recursive":      []any{},
			},
			"pids_stats": map[string]any{
				"current": 0,
			},
		}

		fmt.Fprintln(w, mustJSON(stats))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		if !stream {
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-time.After(1 * time.Second):
		}
	}
}

// Attach handles POST /containers/{id}/attach.
func (h *Handler) Attach(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusSwitchingProtocols)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
