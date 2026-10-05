// Package exec implements the Docker Engine exec endpoints:
//
//	POST /containers/{id}/exec -> create an exec instance
//	POST /exec/{id}/start      -> run it, streaming over the hijacked connection
//	POST /exec/{id}/resize     -> resize its TTY
//	GET  /exec/{id}/json       -> inspect it, including its exit code
package exec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/rs/zerolog/log"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/internal/streams"
	"github.com/portainer/d2k/pkg/httputils"
)

// finishedRetention is how long a finished exec instance stays inspectable.
const finishedRetention = 10 * time.Minute

// Handler holds the exec instances, which live in memory as in the daemon.
type Handler struct {
	adapter   *adapter.KubernetesDockerAdapter
	mu        sync.Mutex
	instances map[string]*instance
}

type instance struct {
	id          string
	containerID string
	opts        adapter.ExecOptions
	resize      *sizeQueue

	mu       sync.Mutex
	started  bool
	running  bool
	exitCode *int
	finished time.Time
}

// NewHandler creates a Handler.
func NewHandler(a *adapter.KubernetesDockerAdapter) *Handler {
	return &Handler{adapter: a, instances: map[string]*instance{}}
}

// Create handles POST /containers/{id}/exec.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req container.ExecCreateRequest
	if err := httputils.ParseJSON(r, &req); err != nil {
		httputils.WriteError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %s", err))
		return
	}
	if len(req.Cmd) == 0 {
		httputils.WriteErrorFrom(w, errdefs.InvalidParameter(errors.New("no exec command specified")))
		return
	}
	// Kubernetes runs exec processes as the container's user, in its working
	// directory and environment: refuse rather than run them differently.
	var unsupported []string
	if req.User != "" {
		unsupported = append(unsupported, "user")
	}
	if req.WorkingDir != "" {
		unsupported = append(unsupported, "working directory")
	}
	if len(req.Env) > 0 {
		unsupported = append(unsupported, "environment variables")
	}
	if req.Privileged {
		unsupported = append(unsupported, "privileged mode")
	}
	if len(unsupported) > 0 {
		httputils.WriteErrorFrom(w, errdefs.InvalidParameter(fmt.Errorf("exec with a custom %s is not supported by d2k", strings.Join(unsupported, ", "))))
		return
	}

	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/exec")
	ctr, err := h.adapter.InspectContainer(r.Context(), name)
	if err != nil {
		httputils.WriteErrorFrom(w, err)
		return
	}
	if !ctr.State.Running {
		httputils.WriteErrorFrom(w, errdefs.Conflict(fmt.Errorf("container %s is not running", ctr.ID)))
		return
	}

	b := make([]byte, 32)
	_, _ = rand.Read(b)
	inst := &instance{
		id:          hex.EncodeToString(b),
		containerID: ctr.ID,
		opts: adapter.ExecOptions{
			Name:         ctr.ID,
			Cmd:          req.Cmd,
			AttachStdin:  req.AttachStdin,
			AttachStdout: req.AttachStdout,
			AttachStderr: req.AttachStderr,
			Tty:          req.Tty,
		},
		resize: newSizeQueue(),
	}
	if req.ConsoleSize != nil {
		inst.resize.push(req.ConsoleSize[0], req.ConsoleSize[1])
	}

	h.mu.Lock()
	for id, old := range h.instances {
		if old.isFinishedBefore(time.Now().Add(-finishedRetention)) {
			delete(h.instances, id)
		}
	}
	h.instances[inst.id] = inst
	h.mu.Unlock()

	httputils.WriteJSON(w, http.StatusCreated, map[string]string{"Id": inst.id})
}

// Start handles POST /exec/{id}/start.
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	inst, ok := h.lookup(w, r)
	if !ok {
		return
	}
	var req container.ExecStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		httputils.WriteError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %s", err))
		return
	}
	if !inst.markStarted() {
		httputils.WriteErrorFrom(w, errdefs.Conflict(fmt.Errorf("exec %s has already been started", inst.id)))
		return
	}
	if req.ConsoleSize != nil {
		inst.resize.push(req.ConsoleSize[0], req.ConsoleSize[1])
	}

	if req.Detach {
		// run logs the error: a detached exec has no client to report it to.
		go func() { _ = h.run(context.Background(), inst, adapter.ExecStreams{Resize: inst.resize}) }()
		w.WriteHeader(http.StatusOK)
		return
	}

	conn, in, err := streams.Hijack(w, r, inst.opts.Tty)
	if err != nil {
		inst.finish(0)
		httputils.WriteErrorFrom(w, err)
		return
	}
	defer func() { _ = conn.Close() }()

	s := adapter.ExecStreams{Resize: inst.resize}
	if inst.opts.AttachStdin {
		s.Stdin = in
	}
	switch {
	case inst.opts.Tty:
		if inst.opts.AttachStdout || inst.opts.AttachStderr {
			s.Stdout = conn
		}
	default:
		if inst.opts.AttachStdout {
			s.Stdout = streams.Multiplexed(conn, streams.Stdout)
		}
		if inst.opts.AttachStderr {
			s.Stderr = streams.Multiplexed(conn, streams.Stderr)
		}
	}
	// After the hijack the request's context is cancelled when the client
	// closes its side, as `docker exec -i` does once stdin is sent; the
	// command still owes its output. The stream ends with the command.
	if err := h.run(context.WithoutCancel(r.Context()), inst, s); err != nil {
		// The client has no other channel for the error: write it to the stream.
		msg := []byte(err.Error() + "\r\n")
		if !inst.opts.Tty {
			_, _ = streams.Multiplexed(conn, streams.Stderr).Write(msg)
		} else {
			_, _ = conn.Write(msg)
		}
	}
}

// run executes the instance and records its exit code.
func (h *Handler) run(ctx context.Context, inst *instance, s adapter.ExecStreams) error {
	code, err := h.adapter.ExecContainer(ctx, inst.opts, s)
	if err != nil {
		log.Warn().Str("context", "ExecAPI").Str("exec_id", inst.id).Err(err).Msg("Exec failed")
		code = 126 // as the daemon reports a command that could not be run
	}
	inst.finish(code)
	inst.resize.close()
	return err
}

// Resize handles POST /exec/{id}/resize.
func (h *Handler) Resize(w http.ResponseWriter, r *http.Request) {
	inst, ok := h.lookup(w, r)
	if !ok {
		return
	}
	height, errH := strconv.ParseUint(r.URL.Query().Get("h"), 10, 16)
	width, errW := strconv.ParseUint(r.URL.Query().Get("w"), 10, 16)
	if errH != nil || errW != nil {
		httputils.WriteErrorFrom(w, errdefs.InvalidParameter(errors.New("invalid terminal size: h and w must be numbers")))
		return
	}
	inst.resize.push(uint(height), uint(width))
	w.WriteHeader(http.StatusOK)
}

// Inspect handles GET /exec/{id}/json.
func (h *Handler) Inspect(w http.ResponseWriter, r *http.Request) {
	inst, ok := h.lookup(w, r)
	if !ok {
		return
	}
	inst.mu.Lock()
	running, exitCode := inst.running, inst.exitCode
	inst.mu.Unlock()

	httputils.WriteJSON(w, http.StatusOK, container.ExecInspectResponse{
		ID:          inst.id,
		Running:     running,
		ExitCode:    exitCode,
		ContainerID: inst.containerID,
		OpenStdin:   inst.opts.AttachStdin,
		OpenStdout:  inst.opts.AttachStdout,
		OpenStderr:  inst.opts.AttachStderr,
		ProcessConfig: &container.ExecProcessConfig{
			Tty:        inst.opts.Tty,
			Entrypoint: inst.opts.Cmd[0],
			Arguments:  inst.opts.Cmd[1:],
		},
	})
}

func (h *Handler) lookup(w http.ResponseWriter, r *http.Request) (*instance, bool) {
	h.mu.Lock()
	inst, ok := h.instances[r.PathValue("id")]
	h.mu.Unlock()
	if !ok {
		httputils.WriteErrorFrom(w, errdefs.NotFound(fmt.Errorf("no such exec instance: %s", r.PathValue("id"))))
	}
	return inst, ok
}

func (i *instance) markStarted() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.started {
		return false
	}
	i.started, i.running = true, true
	return true
}

func (i *instance) finish(code int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.running, i.exitCode, i.finished = false, &code, time.Now()
}

func (i *instance) isFinishedBefore(t time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.exitCode != nil && i.finished.Before(t)
}

// sizeQueue feeds terminal resizes to the exec stream. It keeps only the
// latest size, as a terminal only cares about its current one.
type sizeQueue struct {
	sizes chan remotecommand.TerminalSize
	once  sync.Once
	done  chan struct{}
}

func newSizeQueue() *sizeQueue {
	return &sizeQueue{sizes: make(chan remotecommand.TerminalSize, 1), done: make(chan struct{})}
}

func (q *sizeQueue) push(height, width uint) {
	size := remotecommand.TerminalSize{Height: uint16(height), Width: uint16(width)}
	select {
	case <-q.done:
		return
	default:
	}
	select {
	case <-q.sizes: // drop a size not yet applied
	default:
	}
	select {
	case q.sizes <- size:
	default:
	}
}

// Next blocks until a resize, or returns nil once the exec has ended.
func (q *sizeQueue) Next() *remotecommand.TerminalSize {
	select {
	case size := <-q.sizes:
		return &size
	case <-q.done:
		return nil
	}
}

func (q *sizeQueue) close() { q.once.Do(func() { close(q.done) }) }
