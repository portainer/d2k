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
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/internal/apispec"
	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/internal/filters"
	"github.com/portainer/d2k/internal/namesgenerator"
	"github.com/portainer/d2k/internal/streams"
	"github.com/portainer/d2k/pkg/httputils"
)

// Handler holds dependencies for all container API endpoints.
type Handler struct {
	adapter *adapter.KubernetesDockerAdapter

	mu       sync.Mutex
	attached map[string]map[*streams.SizeQueue]bool // TTY sizes of attach sessions, by container ID
}

// NewHandler creates a Handler.
func NewHandler(a *adapter.KubernetesDockerAdapter) *Handler {
	return &Handler{adapter: a, attached: map[string]map[*streams.SizeQueue]bool{}}
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
	case strings.HasSuffix(path, "/resize"):
		h.Resize(w, r)
	case strings.HasSuffix(path, "/rename"):
		h.Rename(w, r)
	default:
		apispec.NotRouted(w, r)
	}
}

// Rename handles POST /containers/{id}/rename.
func (h *Handler) Rename(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "/rename")
	newName := r.URL.Query().Get("name")
	if err := h.adapter.RenameContainer(r.Context(), name, newName); err != nil {
		log.Error().Str("context", "ContainersAPI").Any("name", name).Any("new_name", newName).Err(err).Msg("RenameContainer failed")
		httputils.WriteErrorFrom(w, err)
		return
	}
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
		apispec.NotRouted(w, r)
	}
}

// List handles GET /containers/json (docker ps / docker ps -a).
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	args, err := httputils.ParseFilters(r, containerFilters)
	if err != nil {
		httputils.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil {
			httputils.WriteErrorFrom(w, errdefs.InvalidParameter(fmt.Errorf("invalid limit %q: %w", v, err)))
			return
		}
	}
	// As in the Docker daemon, a status filter or a limit implies all containers.
	all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true" || args.Contains("status") || limit > 0

	ctrs, err := h.adapter.ListContainers(r.Context(), all)
	if err != nil {
		log.Error().Str("context", "ContainersAPI").Err(err).Msg("ListContainers failed")
		httputils.WriteErrorFrom(w, err)
		return
	}
	ctrs = filterContainers(ctrs, args)
	slices.SortStableFunc(ctrs, func(a, b adapter.ContainerSummary) int { return cmp.Compare(b.Created, a.Created) })
	if limit > 0 && len(ctrs) > limit {
		ctrs = ctrs[:limit]
	}

	type endpoint struct {
		NetworkID string `json:"NetworkID"`
	}
	type item struct {
		ID        string                  `json:"Id"`
		Names     []string                `json:"Names"`
		Image     string                  `json:"Image"`
		ImageID   string                  `json:"ImageID"`
		Status    string                  `json:"Status"`
		State     string                  `json:"State"`
		Created   int64                   `json:"Created"`
		Ports     []container.PortSummary `json:"Ports"`
		Labels    map[string]string       `json:"Labels"`
		IPAddress string                  `json:"IPAddress"`

		NetworkSettings struct {
			Networks map[string]endpoint `json:"Networks"`
		} `json:"NetworkSettings"`
		Mounts []container.MountPoint `json:"Mounts"`
	}

	result := make([]item, 0, len(ctrs))
	for _, c := range ctrs {
		it := item{
			ID:        c.ID,
			Names:     c.Names,
			Image:     c.Image,
			ImageID:   c.ImageID,
			Status:    c.Status,
			State:     c.State,
			Created:   c.Created,
			Labels:    c.Labels,
			IPAddress: c.IPAddress,
			Ports:     c.Ports,
			Mounts:    c.Mounts,
		}
		it.NetworkSettings.Networks = map[string]endpoint{}
		for name, id := range c.Networks {
			it.NetworkSettings.Networks[name] = endpoint{NetworkID: id}
		}
		result = append(result, it)
	}

	httputils.WriteJSON(w, http.StatusOK, result)
}

// containerFilters are the `filters` keys d2k supports on GET /containers/json.
var containerFilters = map[string]bool{"id": true, "name": true, "label": true, "status": true, "network": true, "volume": true}

// filterContainers applies list filters with the Docker daemon's matching rules.
func filterContainers(ctrs []adapter.ContainerSummary, args filters.Args) []adapter.ContainerSummary {
	result := []adapter.ContainerSummary{}
	for _, c := range ctrs {
		if args.Contains("name") && !slices.ContainsFunc(c.Names, func(n string) bool {
			return args.Match("name", n) || args.Match("name", strings.TrimPrefix(n, "/"))
		}) {
			continue
		}
		if !args.Match("id", c.ID) || !args.MatchKVList("label", c.Labels) || !args.Match("status", c.State) {
			continue
		}
		if args.Contains("network") && !slices.ContainsFunc(args.Get("network"), func(v string) bool {
			if _, ok := c.Networks[v]; ok {
				return true
			}
			for _, id := range c.Networks {
				if strings.HasPrefix(id, v) {
					return true
				}
			}
			return false
		}) {
			continue
		}
		if args.Contains("volume") && !slices.ContainsFunc(args.Get("volume"), func(v string) bool {
			return slices.ContainsFunc(c.Mounts, func(m container.MountPoint) bool {
				return m.Name == v || m.Destination == v
			})
		}) {
			continue
		}
		result = append(result, c)
	}
	return result
}

// Create handles POST /containers/create (docker run — create phase).
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = strings.ReplaceAll(namesgenerator.GetRandomName(0), "_", "-")
	}

	var req container.CreateRequest
	if err := httputils.ParseJSON(r, &req); err != nil {
		httputils.WriteError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %s", err))
		return
	}
	opts, ignored := adapter.RunOptionsFromRequest(name, req)
	if platform := r.URL.Query().Get("platform"); platform != "" {
		ignored = append(ignored, fmt.Sprintf("platform %q ignored: the node's container runtime picks the image platform", platform))
	}

	id, warnings, err := h.adapter.CreateContainer(r.Context(), opts)
	if err != nil {
		log.Error().Str("context", "ContainersAPI").Any("name", name).Err(err).Msg("CreateContainer failed")
		httputils.WriteErrorFrom(w, err)
		return
	}

	httputils.WriteJSON(w, http.StatusCreated, map[string]any{
		"Id":       id,
		"Warnings": append(ignored, warnings...),
	})
}

// PutArchive handles PUT /containers/{id}/archive: it writes the files of a
// tar archive into the container under the directory named by path, as
// `docker cp` and compose's secrets and configs do.
func (h *Handler) PutArchive(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	if dir == "" {
		httputils.WriteErrorFrom(w, errdefs.InvalidParameter(errors.New("path is required")))
		return
	}
	if err := h.adapter.CopyToContainer(r.Context(), r.PathValue("id"), dir, r.Body); err != nil {
		log.Error().Str("context", "ContainersAPI").Str("id", r.PathValue("id")).Err(err).Msg("CopyToContainer failed")
		httputils.WriteErrorFrom(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Wait handles POST /containers/{id}/wait: it answers when the container
// stops running (condition not-running, the default), next exits
// (next-exit) or is removed (removed), with its exit code.
func (h *Handler) Wait(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "/wait")
	condition := r.URL.Query().Get("condition")
	if !slices.Contains([]string{"", "not-running", "next-exit", "removed"}, condition) {
		httputils.WriteErrorFrom(w, errdefs.InvalidParameter(fmt.Errorf("invalid condition %q", condition)))
		return
	}
	// Subscribe before looking at the state so no exit is missed between.
	_, live, cancel := h.adapter.SubscribeEvents()
	defer cancel()
	ctr, err := h.adapter.InspectContainer(r.Context(), name)
	if err != nil {
		httputils.WriteErrorFrom(w, err)
		return
	}
	// A container that is starting is running, as far as the client knows:
	// wait for its run to begin. A --rm run that ends before it is seen
	// running is removed meanwhile: its die and destroy events answer.
	starting := adapter.IsStarting(ctr.State)
	if starting {
		if _, ok := waitUntilStarted(r.Context(), h.adapter, ctr.ID, ""); ok {
			if ctr, err = h.adapter.InspectContainer(r.Context(), name); err != nil {
				httputils.WriteErrorFrom(w, err)
				return
			}
			starting = false
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	respond := func(code int) { _ = json.NewEncoder(w).Encode(container.WaitResponse{StatusCode: int64(code)}) }

	if (condition == "" || condition == "not-running") && !ctr.State.Running && !starting {
		respond(ctr.State.ExitCode)
		return
	}
	// A container removed after its run ends answers with the run's code.
	exitCode := ctr.State.ExitCode
	for {
		select {
		case <-r.Context().Done():
			return
		case m := <-live:
			if m.Actor.ID != ctr.ID {
				continue
			}
			switch {
			case m.Action == events.ActionDestroy:
				respond(exitCode)
				return
			case m.Action == events.ActionDie:
				exitCode, _ = strconv.Atoi(m.Actor.Attributes["exitCode"])
				if condition != "removed" {
					respond(exitCode)
					return
				}
			case condition != "removed" && m.Action == events.ActionStop:
				respond(0)
				return
			}
		}
	}
}

// Start handles POST /containers/{id}/start.
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "/start")
	if err := h.adapter.StartContainer(r.Context(), name); errdefs.IsNotModified(err) {
		// Already running: Docker answers 304.
		w.WriteHeader(http.StatusNotModified)
		return
	} else if err != nil {
		log.Error().Str("context", "ContainersAPI").Any("name", name).Err(err).Msg("StartContainer failed")
		httputils.WriteErrorFrom(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Stop handles POST /containers/{id}/stop.
func (h *Handler) Stop(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "/stop")
	timeout, err := timeoutParam(r)
	if err != nil {
		httputils.WriteErrorFrom(w, err)
		return
	}
	if err := h.adapter.StopContainer(r.Context(), name, timeout); errdefs.IsNotModified(err) {
		// Already stopped: Docker answers 304.
		w.WriteHeader(http.StatusNotModified)
		return
	} else if err != nil {
		log.Error().Str("context", "ContainersAPI").Any("name", name).Err(err).Msg("StopContainer failed")
		httputils.WriteErrorFrom(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Restart handles POST /containers/{id}/restart.
func (h *Handler) Restart(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "/restart")
	timeout, err := timeoutParam(r)
	if err != nil {
		httputils.WriteErrorFrom(w, err)
		return
	}
	if err := h.adapter.RestartContainer(r.Context(), name, timeout); err != nil {
		log.Error().Str("context", "ContainersAPI").Any("name", name).Err(err).Msg("RestartContainer failed")
		httputils.WriteErrorFrom(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Remove handles DELETE /containers/{id}.
func (h *Handler) Remove(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "")
	if !boolParam(r.URL.Query().Get("force")) {
		ctr, err := h.adapter.InspectContainer(r.Context(), name)
		if err != nil {
			httputils.WriteErrorFrom(w, err)
			return
		}
		if ctr.State.Running || adapter.IsStarting(ctr.State) {
			httputils.WriteErrorFrom(w, errdefs.Conflict(fmt.Errorf("cannot remove container %q: container is %s: stop the container before removing or force remove", name, ctr.State.Status)))
			return
		}
	}
	if err := h.adapter.RemoveContainer(r.Context(), name); err != nil {
		log.Error().Str("context", "ContainersAPI").Any("name", name).Err(err).Msg("RemoveContainer failed")
		httputils.WriteErrorFrom(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Inspect handles GET /containers/{id}/json.
func (h *Handler) Inspect(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "/json")
	result, err := h.adapter.InspectContainer(r.Context(), name)
	if err != nil {
		log.Error().Str("context", "ContainersAPI").Any("name", name).Err(err).Msg("InspectContainer failed")
		httputils.WriteErrorFrom(w, err)
		return
	}
	httputils.WriteJSON(w, http.StatusOK, result)
}

// Logs handles GET /containers/{id}/logs.
func (h *Handler) Logs(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "/logs")
	q := r.URL.Query()
	stdout, stderr := boolParam(q.Get("stdout")), boolParam(q.Get("stderr"))
	if !stdout && !stderr {
		httputils.WriteErrorFrom(w, errdefs.InvalidParameter(errors.New("bad parameters: you must choose at least one stream")))
		return
	}
	since, errSince := httputils.ParseUnixTime(q.Get("since"))
	until, errUntil := httputils.ParseUnixTime(q.Get("until"))
	if err := errors.Join(errSince, errUntil); err != nil {
		httputils.WriteErrorFrom(w, errdefs.InvalidParameter(err))
		return
	}

	// Timestamps are always requested: they bound "until", and are removed
	// unless the client asked for them.
	logs, tty, err := h.adapter.GetContainerLogs(r.Context(), name, adapter.LogOptions{
		Follow:     boolParam(q.Get("follow")),
		Timestamps: true,
		Tail:       q.Get("tail"),
		Since:      since,
	})
	if err != nil {
		log.Error().Str("context", "ContainersAPI").Str("name", name).Err(err).Msg("Unable to read container logs")
		httputils.WriteErrorFrom(w, err)
		return
	}
	defer func() { _ = logs.Close() }()

	// Kubernetes keeps one log for stdout and stderr: it goes to the
	// requested stream. A TTY container's log is a raw stream, as in Docker.
	var out io.Writer = w
	contentType := "application/vnd.docker.raw-stream"
	if !tty {
		contentType = "application/vnd.docker.multiplexed-stream"
		stream := streams.Stdout
		if !stdout {
			stream = streams.Stderr
		}
		out = streams.Multiplexed(w, stream)
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	err = copyLogLines(logs, until, boolParam(q.Get("timestamps")), func(line []byte) error {
		if _, err := out.Write(line); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Warn().Str("context", "ContainersAPI").Str("name", name).Err(err).Msg("Log stream ended")
	}
}

// copyLogLines emits timestamped log lines one by one, stopping at the first
// line after until (when set), and removing each line's timestamp unless
// keepTimestamps.
func copyLogLines(src io.Reader, until time.Time, keepTimestamps bool, emit func([]byte) error) error {
	r := bufio.NewReader(src)
	for {
		line, readErr := r.ReadBytes('\n')
		if len(line) > 0 {
			stamp, rest, _ := bytes.Cut(line, []byte(" "))
			if ts, err := time.Parse(time.RFC3339Nano, string(stamp)); err == nil {
				if !until.IsZero() && ts.After(until) {
					return nil
				}
				if !keepTimestamps {
					line = rest
				}
			}
			if err := emit(line); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func boolParam(v string) bool { return v == "1" || v == "true" }

// timeoutParam reads the t parameter of stop and restart: the seconds the
// container has to stop before it is killed, nil when not given. Their signal
// parameter has no Kubernetes equivalent: the pods get SIGTERM.
func timeoutParam(r *http.Request) (*int, error) {
	v := r.URL.Query().Get("t")
	if v == "" {
		return nil, nil
	}
	t, err := strconv.Atoi(v)
	if err != nil {
		return nil, errdefs.InvalidParameter(fmt.Errorf("invalid timeout %q: %w", v, err))
	}
	return &t, nil
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

// Stats handles GET /containers/{id}/stats: one sample, or one a second
// while stream is set.
func (h *Handler) Stats(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "/stats")
	stream := r.URL.Query().Get("stream") != "0" && r.URL.Query().Get("stream") != "false"
	ctr, err := h.adapter.InspectContainer(r.Context(), name)
	if err != nil {
		httputils.WriteErrorFrom(w, err)
		return
	}

	// The first sample is read before answering: a failure to read it is
	// the request's error, not a 200 without a body.
	u, err := h.adapter.ContainerUsage(r.Context(), ctr.ID)
	if err != nil {
		httputils.WriteErrorFrom(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	var clock cpuClock
	for first := true; ; first = false {
		if !first {
			if u, err = h.adapter.ContainerUsage(r.Context(), ctr.ID); err != nil {
				log.Warn().Str("context", "ContainersAPI").Str("name", name).Err(err).Msg("Unable to read container usage")
				return
			}
		}
		s := clock.sample(u.CPUNanoCores, u.OnlineCPUs, time.Now())
		s.ID, s.Name, s.OSType = ctr.ID, ctr.Name, "linux"
		s.MemoryStats = container.MemoryStats{Usage: uint64(u.MemoryBytes), Limit: uint64(u.MemoryLimit)}
		if err := enc.Encode(s); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		if !stream {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// cpuClock turns Kubernetes' CPU rate into Docker's cumulative counters.
// The Docker CLI computes CPU% as the change in a container's CPU time over
// the change in the host's (CPU count x elapsed time), x CPUs x 100: with
// both counters advanced from the rate, that gives back the rate.
type cpuClock struct {
	total, system uint64
	at            time.Time
}

// sample advances the counters to now at nanoCores and returns them with the
// previous sample. The first sample's previous one is a second earlier at
// the same rate, as the daemon's two reads for a one-off sample are.
func (c *cpuClock) sample(nanoCores int64, cpus int, now time.Time) container.StatsResponse {
	if c.at.IsZero() {
		c.at = now.Add(-time.Second)
	}
	prev := container.CPUStats{CPUUsage: container.CPUUsage{TotalUsage: c.total}, SystemUsage: c.system, OnlineCPUs: uint32(cpus)}
	preRead := c.at
	elapsed := now.Sub(c.at)
	c.total += uint64(float64(nanoCores) * elapsed.Seconds())
	c.system += uint64(elapsed.Nanoseconds()) * uint64(cpus)
	c.at = now
	return container.StatsResponse{
		Read:        now,
		PreRead:     preRead,
		CPUStats:    container.CPUStats{CPUUsage: container.CPUUsage{TotalUsage: c.total}, SystemUsage: c.system, OnlineCPUs: uint32(cpus)},
		PreCPUStats: prev,
	}
}

// Attach handles POST /containers/{id}/attach. With stdin it attaches to
// the container's process. Without, it follows the container's log: the
// Docker CLI attaches before starting a container, and a process attach
// would miss what the container prints before the attach connects.
func (h *Handler) Attach(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "/attach")
	q := r.URL.Query()
	stdin, stdout, stderr := boolParam(q.Get("stdin")), boolParam(q.Get("stdout")), boolParam(q.Get("stderr"))
	stream, withLogs := boolParam(q.Get("stream")), boolParam(q.Get("logs"))

	// Subscribe before inspecting so a die between the two is not missed.
	_, live, cancelEvents := h.adapter.SubscribeEvents()
	defer cancelEvents()

	ctr, err := h.adapter.InspectContainer(r.Context(), name)
	if err != nil {
		httputils.WriteErrorFrom(w, err)
		return
	}
	defer h.adapter.Attaching(ctr.ID)()
	tty := ctr.Config.Tty
	conn, in, err := streams.Hijack(w, r, tty)
	if err != nil {
		httputils.WriteErrorFrom(w, err)
		return
	}
	defer func() { _ = conn.Close() }()
	if !stream && !withLogs {
		return
	}

	var out, errOut io.Writer
	switch {
	case tty:
		out, errOut = conn, conn
	default:
		out, errOut = streams.Multiplexed(conn, streams.Stdout), streams.Multiplexed(conn, streams.Stderr)
	}
	if !stdout {
		out = nil
	}
	if !stderr {
		errOut = nil
	}

	// The attach outlives the request's context, which Go cancels when the
	// client half-closes after sending stdin; it ends with the process.
	ctx, stop := context.WithCancel(context.WithoutCancel(r.Context()))
	defer stop()
	// A pod force-deleted or lost out from under the stream never gets the
	// kubelet to end it (D2K-038): cancel ctx shortly after the container's
	// own die event arrives, the same event Wait relies on, rather than wait
	// on a stream that may never return on its own.
	go stopOnDeath(ctx, stop, ctr.ID, live, deathGrace)
	// Register the session first: the Docker CLI resizes right after start,
	// before the container runs; the queue keeps that size for the attach.
	var resize *streams.SizeQueue
	if stdin && stream {
		resize = h.attachSession(ctr.ID)
		defer h.endAttachSession(ctr.ID, resize)
	}
	wasRunning, running := ctr.State.Running, ctr.State.Running
	if !wasRunning {
		var ok bool
		if running, ok = waitUntilStarted(ctx, h.adapter, ctr.ID, ctr.State.StartedAt); !ok {
			return
		}
	}

	// A run that already ended has no process to attach to: its output is
	// sent below.
	if stdin && stream && running {
		var sent atomic.Int64
		err := h.adapter.AttachContainer(ctx, ctr.ID, tty, adapter.ExecStreams{Stdin: in, Stdout: counting(out, &sent), Stderr: counting(errOut, &sent), Resize: resize})
		if err != nil {
			log.Warn().Str("context", "ContainersAPI").Str("name", name).Err(err).Msg("Attach ended")
		}
		// A run can end between being seen running and the process attach
		// connecting, which the kubelet then refuses: its output is then
		// only in its log.
		if wasRunning || sent.Load() > 0 {
			return
		}
	} else {
		// Without stdin the client sends nothing, and the Docker CLI
		// half-closes its side at once: EOF is not a disconnect. A read
		// error is; so is a failed write of the next log line.
		go func() {
			if _, err := io.Copy(io.Discard, in); err != nil {
				stop()
			}
		}()
	}
	tail := "0"
	if withLogs || !wasRunning {
		tail = "all"
	}
	logs, _, err := h.adapter.GetContainerLogs(ctx, ctr.ID, adapter.LogOptions{Follow: stream, Tail: tail})
	if err != nil {
		log.Warn().Str("context", "ContainersAPI").Str("name", name).Err(err).Msg("Attach ended")
		return
	}
	defer func() { _ = logs.Close() }()
	if out == nil {
		out = errOut
	}
	if out != nil {
		_, _ = io.Copy(out, logs)
	}
}

// countingWriter counts the bytes written to w in n.
type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// counting returns w counting its bytes in n, or nil for no writer.
func counting(w io.Writer, n *atomic.Int64) io.Writer {
	if w == nil {
		return nil
	}
	return countingWriter{w, n}
}

// waitUntilStarted polls until the container runs, or a run started after
// startedAt has already ended, as a short run of a container that runs once
// may before it is seen running. ok is false if the container is removed or
// ctx ends first.
func waitUntilStarted(ctx context.Context, a *adapter.KubernetesDockerAdapter, id, startedAt string) (running, ok bool) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		ctr, err := a.InspectContainer(ctx, id)
		if err != nil {
			return false, false
		}
		if ctr.State.Running {
			return true, true
		}
		if ctr.State.Status == container.StateExited && ctr.State.StartedAt != startedAt {
			return false, true
		}
		select {
		case <-ctx.Done():
			return false, false
		case <-ticker.C:
		}
	}
}

// deathGrace is how long an attach has to end on its own once its container
// dies. The die usually arrives before the attach has sent the run's output
// (D2K-050): docker run attaches first, and a short run dies before it is
// seen running. The kubelet ends a log follow about a second after the
// container exits.
const deathGrace = 10 * time.Second

// stopOnDeath cancels an attach grace after id's container dies, so a
// stream the kubelet will never end on its own (the pod's own force-delete
// or loss) still returns, instead of leaving the attach hanging after
// inspect and wait already report the container dead (D2K-038).
func stopOnDeath(ctx context.Context, stop context.CancelFunc, id string, live <-chan events.Message, grace time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-live:
			if m.Actor.ID == id && m.Action == events.ActionDie {
				select {
				case <-ctx.Done():
				case <-time.After(grace):
					stop()
				}
				return
			}
		}
	}
}

// Resize handles POST /containers/{id}/resize: it resizes the TTY of the
// container's attach sessions, if any.
func (h *Handler) Resize(w http.ResponseWriter, r *http.Request) {
	name := containerName(r.URL.Path, "/resize")
	height, errH := strconv.ParseUint(r.URL.Query().Get("h"), 10, 16)
	width, errW := strconv.ParseUint(r.URL.Query().Get("w"), 10, 16)
	if errH != nil || errW != nil {
		httputils.WriteErrorFrom(w, errdefs.InvalidParameter(errors.New("invalid terminal size: h and w must be numbers")))
		return
	}
	ctr, err := h.adapter.InspectContainer(r.Context(), name)
	if err != nil {
		httputils.WriteErrorFrom(w, err)
		return
	}
	h.mu.Lock()
	for resize := range h.attached[ctr.ID] {
		resize.Push(uint(height), uint(width))
	}
	h.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) attachSession(id string) *streams.SizeQueue {
	h.mu.Lock()
	defer h.mu.Unlock()
	q := streams.NewSizeQueue()
	if h.attached[id] == nil {
		h.attached[id] = map[*streams.SizeQueue]bool{}
	}
	h.attached[id][q] = true
	return q
}

func (h *Handler) endAttachSession(id string, q *streams.SizeQueue) {
	h.mu.Lock()
	delete(h.attached[id], q)
	if len(h.attached[id]) == 0 {
		delete(h.attached, id)
	}
	h.mu.Unlock()
	q.Close()
}
