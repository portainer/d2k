package exec

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"go.uber.org/zap"
	"github.com/google/uuid"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/pkg/httputils"
)

// Handler holds dependencies for exec API endpoints.
type Handler struct {
	adapter   *adapter.KubernetesDockerAdapter
	logger    *zap.SugaredLogger
	mu        sync.Mutex
	instances map[string]*execInstance
}

type execInstance struct {
	containerID string
	opts        adapter.ExecOptions
}

// NewHandler creates a Handler.
func NewHandler(a *adapter.KubernetesDockerAdapter, logger *zap.SugaredLogger) *Handler {
	return &Handler{
		adapter:   a,
		logger:    logger,
		instances: map[string]*execInstance{},
	}
}

// createBody mirrors the Docker exec create request body.
type createBody struct {
	Cmd          []string `json:"Cmd"`
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
}

// Create handles POST /containers/{id}/exec.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	containerID := containerIDFromPath(r.URL.Path)

	var body createBody
	if err := httputils.ParseJSON(r, &body); err != nil {
		httputils.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	id := uuid.New().String()

	h.mu.Lock()
	h.instances[id] = &execInstance{
		containerID: containerID,
		opts: adapter.ExecOptions{
			Name:         containerID,
			Cmd:          body.Cmd,
			AttachStdin:  body.AttachStdin,
			AttachStdout: body.AttachStdout,
			AttachStderr: body.AttachStderr,
			Tty:          body.Tty,
		},
	}
	h.mu.Unlock()

	httputils.WriteJSON(w, http.StatusCreated, map[string]any{
		"Id": id,
	})
}

// startBody mirrors the Docker exec start request body.
type startBody struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
}

// Start handles POST /exec/{id}/start.
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	id := execIDFromPath(r.URL.Path)

	h.mu.Lock()
	instance, ok := h.instances[id]
	h.mu.Unlock()

	if !ok {
		httputils.WriteError(w, http.StatusNotFound, fmt.Sprintf("exec instance %q not found", id))
		return
	}

	var body startBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		httputils.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
	w.Header().Set("Connection", "upgrade")
	w.Header().Set("Upgrade", "tcp")
	w.WriteHeader(http.StatusSwitchingProtocols)

	// Use a pipe to bridge the multiplexed Docker stream to plain io.Writer.
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()

	// Copy request body to stdin pipe.
	go func() {
		defer stdinW.Close()
		io.Copy(stdinW, r.Body)
	}()

	// Forward stdout with Docker multiplexed framing.
	go func() {
		defer stdoutR.Close()
		streamToWriter(w, 1, stdoutR)
	}()

	// Forward stderr with Docker multiplexed framing.
	go func() {
		defer stderrR.Close()
		streamToWriter(w, 2, stderrR)
	}()

	opts := instance.opts
	if err := h.adapter.ExecContainer(r.Context(), opts, stdinR, stdoutW, stderrW); err != nil {
		h.logger.Warnw("exec stream ended", "id", id, "error", err)
	}

	stdoutW.Close()
	stderrW.Close()
}

// Inspect handles GET /exec/{id}/json.
func (h *Handler) Inspect(w http.ResponseWriter, r *http.Request) {
	id := execIDFromPath(r.URL.Path)

	h.mu.Lock()
	instance, ok := h.instances[id]
	h.mu.Unlock()

	if !ok {
		httputils.WriteError(w, http.StatusNotFound, fmt.Sprintf("exec instance %q not found", id))
		return
	}

	httputils.WriteJSON(w, http.StatusOK, map[string]any{
		"ID":          id,
		"ContainerID": instance.containerID,
		"Running":     false,
		"ExitCode":    0,
		"ProcessConfig": map[string]any{
			"entrypoint": instance.opts.Cmd[0],
			"arguments":  instance.opts.Cmd[1:],
			"tty":        instance.opts.Tty,
		},
	})
}

// streamToWriter reads from r and writes to w with Docker multiplexed framing.
func streamToWriter(w io.Writer, streamType byte, r io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			header := []byte{
				streamType,
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

func containerIDFromPath(path string) string {
	// /containers/{id}/exec
	parts := strings.Split(strings.TrimPrefix(path, "/containers/"), "/")
	return parts[0]
}

func execIDFromPath(path string) string {
	// /exec/{id}/start or /exec/{id}/json
	parts := strings.Split(strings.TrimPrefix(path, "/exec/"), "/")
	return parts[0]
}
