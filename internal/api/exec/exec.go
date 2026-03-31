package exec

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"crypto/rand"
	"encoding/hex"
	
	"go.uber.org/zap"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/pkg/httputils"
	"github.com/gorilla/websocket"
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

	b := make([]byte, 32)
	rand.Read(b)
	id := hex.EncodeToString(b)

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

	// Detect WebSocket upgrade (Portainer browser console).
	if websocket.IsWebSocketUpgrade(r) {
		h.startWebSocket(w, r, instance)
		return
	}

	// Raw TCP hijack (Docker CLI).
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		httputils.WriteError(w, http.StatusInternalServerError, "connection hijacking not supported")
		return
	}

	conn, brw, err := hijacker.Hijack()
	if err != nil {
		httputils.WriteError(w, http.StatusInternalServerError, fmt.Sprintf("hijack failed: %s", err))
		return
	}
	defer conn.Close()

	if instance.opts.Tty {
		_, err = conn.Write([]byte("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"))
	} else {
		_, err = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"))
	}
	if err != nil {
		return
	}

	opts := instance.opts
	if err := h.adapter.ExecContainer(r.Context(), opts, brw, brw, brw); err != nil {
		h.logger.Warnw("exec stream ended", "id", id, "error", err)
	}
}

// startWebSocket handles the Portainer browser console path.
// Portainer connects via WebSocket and uses a simple framing protocol:
// - Incoming frames: first byte is stream type (0=stdin), rest is data
// - Outgoing frames: first byte is stream type (1=stdout, 2=stderr), rest is data
func (h *Handler) startWebSocket(w http.ResponseWriter, r *http.Request, instance *execInstance) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Errorw("WebSocket upgrade failed", "error", err)
		return
	}
	defer wsConn.Close()

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()

	// WebSocket → stdin
	go func() {
		defer stdinW.Close()
		for {
			_, msg, err := wsConn.ReadMessage()
			if err != nil {
				return
			}
			if len(msg) > 1 {
				// First byte is stream type, rest is data.
				stdinW.Write(msg[1:])
			}
		}
	}()

	// stdout → WebSocket
	go func() {
		defer stdoutR.Close()
		buf := make([]byte, 32*1024)
		for {
			n, err := stdoutR.Read(buf)
			if n > 0 {
				frame := append([]byte{1}, buf[:n]...)
				wsConn.WriteMessage(websocket.BinaryMessage, frame)
			}
			if err != nil {
				return
			}
		}
	}()

	// stderr → WebSocket
	go func() {
		defer stderrR.Close()
		buf := make([]byte, 32*1024)
		for {
			n, err := stderrR.Read(buf)
			if n > 0 {
				frame := append([]byte{2}, buf[:n]...)
				wsConn.WriteMessage(websocket.BinaryMessage, frame)
			}
			if err != nil {
				return
			}
		}
	}()

	opts := instance.opts
	if err := h.adapter.ExecContainer(r.Context(), opts, stdinR, stdoutW, stderrW); err != nil {
		h.logger.Warnw("WebSocket exec stream ended", "error", err)
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

	entrypoint := ""
	args := []string{}
	if len(instance.opts.Cmd) > 0 {
		entrypoint = instance.opts.Cmd[0]
	}
	if len(instance.opts.Cmd) > 1 {
		args = instance.opts.Cmd[1:]
	}

	httputils.WriteJSON(w, http.StatusOK, map[string]any{
		"ID":          id,
		"ContainerID": instance.containerID,
		"Running":     false,
		"ExitCode":    0,
		"OpenStdin":   instance.opts.AttachStdin,
		"OpenStdout":  instance.opts.AttachStdout,
		"OpenStderr":  instance.opts.AttachStderr,
		"ProcessConfig": map[string]any{
			"entrypoint": entrypoint,
			"arguments":  args,
			"tty":        instance.opts.Tty,
			"privileged": false,
			"user":       "",
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
	parts := strings.Split(strings.TrimPrefix(path, "/containers/"), "/")
	return parts[0]
}

func execIDFromPath(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/exec/"), "/")
	return parts[0]
}
