// Package swarm implements the Docker Swarm API surface for d2k.
//
// d2k translates Swarm API calls into Kubernetes operations within a single
// namespace. Stack identity is carried by labels; Swarm-format IDs are derived
// deterministically from Kubernetes resource UIDs. No separate namespace per
// stack - all resources share the namespace d2k was configured with.
//
// Implemented endpoints:
//
//	GET  /swarm                  -> cluster identity (stubbed from ConfigMap)
//	POST /swarm/init             -> no-op, cluster already exists
//	GET  /nodes                  -> list Kubernetes nodes as Swarm nodes
//	GET  /nodes/{id}             -> inspect single node
//	POST /nodes/{id}/update      -> drain -> cordon+drain, active -> uncordon
//	POST /services/create        -> Compose service -> Deployment + Service
//	GET  /services               -> list Deployments as Swarm services
//	GET  /services/{id}          -> inspect single service
//	POST /services/{id}/update   -> scale / image / env update
//	DELETE /services/{id}        -> delete Deployment + k8s Service
//	GET  /services/{id}/logs     -> fan-out to all Pods, multiplex streams
//	GET  /tasks                  -> list Pods as Swarm tasks
//	GET  /tasks/{id}             -> inspect single task
//	POST /secrets/create         -> create Kubernetes Secret
//	GET  /secrets                -> list d2k Secrets
//	GET  /secrets/{id}           -> inspect Secret
//	DELETE /secrets/{id}         -> delete Secret
//	POST /configs/create         -> create Kubernetes ConfigMap
//	GET  /configs                -> list d2k ConfigMaps
//	GET  /configs/{id}           -> inspect ConfigMap
//	DELETE /configs/{id}         -> delete ConfigMap
//	GET  /distribution/{name}/json -> stub for stack deploy clients
package swarm

import (
	"encoding/json"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/pkg/httputils"
)

// Handler holds dependencies for Swarm API endpoints.
type Handler struct {
	adapter   *adapter.KubernetesDockerAdapter
	namespace string
	logger    *zap.SugaredLogger
}

// NewHandler creates a Handler.
func NewHandler(a *adapter.KubernetesDockerAdapter, namespace string, logger *zap.SugaredLogger) *Handler {
	return &Handler{
		adapter:   a,
		namespace: namespace,
		logger:    logger,
	}
}

// --- /swarm ---

// InspectSwarm handles GET /swarm.
// Returns the stable cluster identity stored in the d2k-identity ConfigMap.
func (h *Handler) InspectSwarm(w http.ResponseWriter, r *http.Request) {
	identity, err := h.adapter.SwarmIdentity(r.Context())
	if err != nil {
		h.logger.Warnw("swarm identity unavailable", "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, "unable to retrieve swarm identity")
		return
	}
	httputils.WriteJSON(w, http.StatusOK, identity)
}

// LeaveSwarm handles POST /swarm/leave.
// This is a single-node cluster backed by Kubernetes - the node cannot leave.
func (h *Handler) LeaveSwarm(w http.ResponseWriter, r *http.Request) {
	httputils.WriteError(w, http.StatusServiceUnavailable, "This node is the last node in the cluster, node cannot leave.")
}

// InitSwarm handles POST /swarm/init.
// This node is already part of a swarm (the Kubernetes cluster) - return
// the standard Docker error so callers know not to re-initialise.
func (h *Handler) InitSwarm(w http.ResponseWriter, r *http.Request) {
	httputils.WriteError(w, http.StatusServiceUnavailable, "This node is already part of a swarm. Use 'docker swarm leave' to leave this swarm and join another one.")
}

// --- /nodes ---

// ListNodes handles GET /nodes.
func (h *Handler) ListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.adapter.SwarmListNodes(r.Context())
	if err != nil {
		h.logger.Warnw("list nodes failed", "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputils.WriteJSON(w, http.StatusOK, nodes)
}

// DispatchNode routes /nodes/{id} and /nodes/{id}/update.
func (h *Handler) DispatchNode(w http.ResponseWriter, r *http.Request) {
	// Path is /nodes/{id} or /nodes/{id}/update
	path := r.URL.Path
	path = strings.TrimPrefix(path, "/nodes/")

	if strings.HasSuffix(path, "/update") {
		id := strings.TrimSuffix(path, "/update")
		h.updateNode(w, r, id)
		return
	}

	switch r.Method {
	case http.MethodGet:
		h.inspectNode(w, r, path)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) inspectNode(w http.ResponseWriter, r *http.Request, id string) {
	node, err := h.adapter.SwarmInspectNode(r.Context(), id)
	if err != nil {
		httputils.WriteError(w, http.StatusNotFound, err.Error())
		return
	}
	httputils.WriteJSON(w, http.StatusOK, node)
}

func (h *Handler) updateNode(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.adapter.SwarmUpdateNode(r.Context(), id, r.Body); err != nil {
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputils.WriteJSON(w, http.StatusOK, map[string]any{})
}

// --- /services ---

// CreateService handles POST /services/create.
func (h *Handler) CreateService(w http.ResponseWriter, r *http.Request) {
	resp, err := h.adapter.SwarmCreateService(r.Context(), r.Body)
	if err != nil {
		h.logger.Warnw("create service failed", "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.logger.Infow("create service response", "resp", resp)
	httputils.WriteJSON(w, http.StatusCreated, resp)
}

// ListServices handles GET /services.
// Supports label filters e.g. docker stack ls sends
// filters={"label":["com.docker.stack.namespace"]} to find stack services.
func (h *Handler) ListServices(w http.ResponseWriter, r *http.Request) {
	services, err := h.adapter.SwarmListServices(r.Context())
	if err != nil {
		h.logger.Warnw("list services failed", "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if rawFilters := r.URL.Query().Get("filters"); rawFilters != "" {
		// Docker CLI sends label filters as either:
		//   {"label":["key=value"]}          (array form)
		//   {"label":{"key":true}}           (map form, e.g. docker stack ls)
		// Parse both forms into a []string slice for filtering.
		var parsed map[string]json.RawMessage
		if err := json.Unmarshal([]byte(rawFilters), &parsed); err == nil {
			if raw, ok := parsed["label"]; ok {
				var labelFilters []string
				// Try array form first.
				if json.Unmarshal(raw, &labelFilters) != nil {
					// Try map form: {"key": true} -> ["key"]
					var labelMap map[string]bool
					if json.Unmarshal(raw, &labelMap) == nil {
						for k := range labelMap {
							labelFilters = append(labelFilters, k)
						}
					}
				}
				if len(labelFilters) > 0 {
					services = filterServicesByLabel(services, labelFilters)
				}
			}
		}
	}

	httputils.WriteJSON(w, http.StatusOK, services)
}

// filterServicesByLabel filters services by required labels.
// Each entry in filters is either "key" (key must exist) or "key=value".
// Labels in the service map are map[string]string (before JSON serialization).
func filterServicesByLabel(services []map[string]any, labelFilters []string) []map[string]any {
	var result []map[string]any
	for _, svc := range services {
		// Extract labels from Spec.Labels - handle both map[string]string
		// and map[string]any (post-JSON-decode) forms.
		labels := map[string]string{}
		if spec, ok := svc["Spec"].(map[string]any); ok {
			switch l := spec["Labels"].(type) {
			case map[string]string:
				for k, v := range l {
					labels[k] = v
				}
			case map[string]any:
				for k, v := range l {
					if vs, ok := v.(string); ok {
						labels[k] = vs
					}
				}
			}
		}
		match := true
		for _, f := range labelFilters {
			if idx := strings.Index(f, "="); idx >= 0 {
				k, v := f[:idx], f[idx+1:]
				if lv, ok := labels[k]; !ok || lv != v {
					match = false
					break
				}
			} else {
				if _, ok := labels[f]; !ok {
					match = false
					break
				}
			}
		}
		if match {
			result = append(result, svc)
		}
	}
	return result
}

// DispatchService routes /services/{id}, /services/{id}/update,
// /services/{id}/logs, /services/{id}/rollback, and DELETE /services/{id}.
func (h *Handler) DispatchService(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/services/")

	switch {
	case strings.HasSuffix(path, "/logs"):
		id := strings.TrimSuffix(path, "/logs")
		h.serviceLogs(w, r, id)
	case strings.HasSuffix(path, "/update"):
		id := strings.TrimSuffix(path, "/update")
		h.updateService(w, r, id)
	case strings.HasSuffix(path, "/rollback"):
		id := strings.TrimSuffix(path, "/rollback")
		h.rollbackService(w, r, id)
	case r.Method == http.MethodDelete:
		h.deleteService(w, r, path)
	case r.Method == http.MethodGet:
		h.inspectService(w, r, path)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) inspectService(w http.ResponseWriter, r *http.Request, id string) {
	svc, err := h.adapter.SwarmInspectService(r.Context(), id)
	if err != nil {
		httputils.WriteError(w, http.StatusNotFound, err.Error())
		return
	}
	httputils.WriteJSON(w, http.StatusOK, svc)
}

func (h *Handler) updateService(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.adapter.SwarmUpdateService(r.Context(), id, r.Body); err != nil {
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Docker API spec requires a JSON body with Warnings array on 200.
	httputils.WriteJSON(w, http.StatusOK, map[string]any{"Warnings": []string{}})
}

func (h *Handler) rollbackService(w http.ResponseWriter, r *http.Request, id string) {
	// Rollback in Kubernetes means reverting to the previous ReplicaSet.
	// We implement this as a no-op that returns success - the service stays
	// running. A proper implementation would use kubectl rollout undo semantics.
	// For now this prevents the CLI from erroring on docker service rollback.
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) deleteService(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.adapter.SwarmDeleteService(r.Context(), id); err != nil {
		httputils.WriteError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) serviceLogs(w http.ResponseWriter, r *http.Request, id string) {
	// Docker log stream multiplexing: each frame has an 8-byte header.
	// Byte 0: stream type (1=stdout, 2=stderr)
	// Bytes 1-3: padding (zero)
	// Bytes 4-7: payload length (big-endian uint32)
	w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	mw := &muxWriter{w: w}
	if err := h.adapter.SwarmServiceLogs(r.Context(), mw, id, r.URL.Query()); err != nil {
		h.logger.Warnw("service logs failed", "service", id, "error", err)
	}
}

// muxWriter wraps an http.ResponseWriter and frames each Write call
// in Docker's 8-byte multiplexed stream header (stdout=1).
type muxWriter struct {
	w http.ResponseWriter
}

func (m *muxWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	header := make([]byte, 8)
	header[0] = 1 // stdout
	l := uint32(len(p))
	header[4] = byte(l >> 24)
	header[5] = byte(l >> 16)
	header[6] = byte(l >> 8)
	header[7] = byte(l)
	if _, err := m.w.Write(header); err != nil {
		return 0, err
	}
	n, err := m.w.Write(p)
	if f, ok := m.w.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

// --- /tasks ---

// ListTasks handles GET /tasks.
// Supports filters:
//   {"service":{"<id>":true}}           -- docker service ps
//   {"_up-to-date":{"true":true},...}    -- progress polling (service filter included)
//   {"label":{"com.docker.stack.namespace=<stack>":true}} -- docker stack ps
func (h *Handler) ListTasks(w http.ResponseWriter, r *http.Request) {
	rawFilters := r.URL.Query().Get("filters")
	serviceFilter, stackFilter := parseTaskFilters(rawFilters)
	tasks, err := h.adapter.SwarmListTasks(r.Context(), serviceFilter, stackFilter)
	if err != nil {
		h.logger.Warnw("list tasks failed", "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.logger.Infow("list tasks", "service_filter", serviceFilter, "stack_filter", stackFilter, "count", len(tasks))
	httputils.WriteJSON(w, http.StatusOK, tasks)
}

// parseTaskFilters extracts service ID and stack name from Docker filter params.
// Service filter: {"service":{"<id>":true}}
// Stack filter:   {"label":{"com.docker.stack.namespace=<stack>":true}}
func parseTaskFilters(raw string) (serviceFilter, stackFilter string) {
	if raw == "" {
		return "", ""
	}
	var filters map[string]map[string]bool
	if err := json.Unmarshal([]byte(raw), &filters); err != nil {
		return "", ""
	}
	for id := range filters["service"] {
		serviceFilter = id
	}
	for label := range filters["label"] {
		const prefix = "com.docker.stack.namespace="
		if strings.HasPrefix(label, prefix) {
			stackFilter = strings.TrimPrefix(label, prefix)
		}
	}
	return serviceFilter, stackFilter
}

// InspectTask handles GET /tasks/{id}.
func (h *Handler) InspectTask(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/tasks/")
	task, err := h.adapter.SwarmInspectTask(r.Context(), id)
	if err != nil {
		httputils.WriteError(w, http.StatusNotFound, err.Error())
		return
	}
	httputils.WriteJSON(w, http.StatusOK, task)
}

// --- /secrets ---

// CreateSecret handles POST /secrets/create.
func (h *Handler) CreateSecret(w http.ResponseWriter, r *http.Request) {
	resp, err := h.adapter.SwarmCreateSecret(r.Context(), r.Body)
	if err != nil {
		h.logger.Warnw("create secret failed", "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputils.WriteJSON(w, http.StatusCreated, resp)
}

// ListSecrets handles GET /secrets.
func (h *Handler) ListSecrets(w http.ResponseWriter, r *http.Request) {
	secrets, err := h.adapter.SwarmListSecrets(r.Context())
	if err != nil {
		h.logger.Warnw("list secrets failed", "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputils.WriteJSON(w, http.StatusOK, secrets)
}

// DispatchSecret routes /secrets/{id} GET and DELETE.
func (h *Handler) DispatchSecret(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/secrets/")
	switch r.Method {
	case http.MethodGet:
		secret, err := h.adapter.SwarmInspectSecret(r.Context(), id)
		if err != nil {
			httputils.WriteError(w, http.StatusNotFound, err.Error())
			return
		}
		httputils.WriteJSON(w, http.StatusOK, secret)
	case http.MethodDelete:
		if err := h.adapter.SwarmDeleteSecret(r.Context(), id); err != nil {
			httputils.WriteError(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// --- /configs ---

// CreateConfig handles POST /configs/create.
func (h *Handler) CreateConfig(w http.ResponseWriter, r *http.Request) {
	resp, err := h.adapter.SwarmCreateConfig(r.Context(), r.Body)
	if err != nil {
		h.logger.Warnw("create config failed", "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputils.WriteJSON(w, http.StatusCreated, resp)
}

// ListConfigs handles GET /configs.
func (h *Handler) ListConfigs(w http.ResponseWriter, r *http.Request) {
	configs, err := h.adapter.SwarmListConfigs(r.Context())
	if err != nil {
		h.logger.Warnw("list configs failed", "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputils.WriteJSON(w, http.StatusOK, configs)
}

// DispatchConfig routes /configs/{id} GET and DELETE.
func (h *Handler) DispatchConfig(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/configs/")
	switch r.Method {
	case http.MethodGet:
		cfg, err := h.adapter.SwarmInspectConfig(r.Context(), id)
		if err != nil {
			httputils.WriteError(w, http.StatusNotFound, err.Error())
			return
		}
		httputils.WriteJSON(w, http.StatusOK, cfg)
	case http.MethodDelete:
		if err := h.adapter.SwarmDeleteConfig(r.Context(), id); err != nil {
			httputils.WriteError(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// --- /stacks ---

// ListStacks handles GET /stacks.
// Portainer calls this directly; docker CLI synthesises it from service labels.
func (h *Handler) ListStacks(w http.ResponseWriter, r *http.Request) {
	stacks, err := h.adapter.SwarmListStacks(r.Context())
	if err != nil {
		h.logger.Warnw("list stacks failed", "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputils.WriteJSON(w, http.StatusOK, stacks)
}

// DeleteStack handles DELETE /stacks/{name}.
func (h *Handler) DeleteStack(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/stacks/")
	if err := h.adapter.SwarmDeleteStack(r.Context(), name); err != nil {
		h.logger.Warnw("delete stack failed", "stack", name, "error", err)
		httputils.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- /distribution ---

// DistributionInspect handles GET /distribution/{name}/json.
// Some stack deploy clients call this to verify image availability.
// We stub it with a minimal valid response.
func (h *Handler) DistributionInspect(w http.ResponseWriter, r *http.Request) {
	httputils.WriteJSON(w, http.StatusOK, map[string]any{
		"Descriptor": map[string]any{
			"MediaType": "application/vnd.docker.distribution.manifest.v2+json",
			"Digest":    "",
			"Size":      0,
		},
		"Platforms": []map[string]any{},
	})
}