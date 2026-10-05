package events

import (
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog/log"

	"github.com/portainer/d2k/internal/adapter"
)

// Handler holds dependencies for the events API endpoint.
type Handler struct {
	adapter *adapter.KubernetesDockerAdapter
}

// NewHandler creates a Handler.
func NewHandler(a *adapter.KubernetesDockerAdapter) *Handler {
	return &Handler{adapter: a}
}

// Stream handles GET /events.
func (h *Handler) Stream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	ch, err := h.adapter.WatchEvents(r.Context())
	if err != nil {
		log.Error().Str("context", "EventsAPI").Err(err).Msg("WatchEvents failed")
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			_, _ = w.Write(b)
			_, _ = w.Write([]byte("\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
}
