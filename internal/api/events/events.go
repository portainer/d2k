// Package events implements GET /events: the namespace's Docker events,
// recorded by the adapter as the daemon records its own.
package events

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/moby/moby/api/types/events"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/internal/filters"
	"github.com/portainer/d2k/pkg/httputils"
)

// eventFilters are the `filters` keys d2k supports on GET /events.
var eventFilters = map[string]bool{"type": true, "event": true, "container": true, "image": true, "label": true, "volume": true}

// Handler serves the events endpoint.
type Handler struct {
	adapter *adapter.KubernetesDockerAdapter
}

// NewHandler creates a Handler.
func NewHandler(a *adapter.KubernetesDockerAdapter) *Handler {
	return &Handler{adapter: a}
}

// Stream handles GET /events. As in Docker: without since or until only new
// events are sent; either replays recorded ones first; until ends the stream.
func (h *Handler) Stream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	args, err := httputils.ParseFilters(r, eventFilters)
	if err != nil {
		httputils.WriteErrorFrom(w, err)
		return
	}
	since, errSince := httputils.ParseUnixTime(q.Get("since"))
	until, errUntil := httputils.ParseUnixTime(q.Get("until"))
	if errSince != nil || errUntil != nil {
		httputils.WriteErrorFrom(w, errdefs.InvalidParameter(firstErr(errSince, errUntil)))
		return
	}

	history, live, cancel := h.adapter.SubscribeEvents()
	defer cancel()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	send := func(m events.Message) {
		if !matches(args, m) {
			return
		}
		_ = enc.Encode(m)
		if flusher != nil {
			flusher.Flush()
		}
	}
	if flusher != nil {
		flusher.Flush()
	}

	inRange := func(m events.Message) bool {
		t := time.Unix(0, m.TimeNano)
		return !t.Before(since) && (until.IsZero() || !t.After(until))
	}
	if q.Get("since") != "" || q.Get("until") != "" {
		for _, m := range history {
			if inRange(m) {
				send(m)
			}
		}
	}

	var deadline <-chan time.Time
	if !until.IsZero() {
		wait := time.Until(until)
		if wait <= 0 {
			return
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline:
			return
		case m := <-live:
			if inRange(m) {
				send(m)
			}
		}
	}
}

// matches applies the event filters as the daemon does.
func matches(args filters.Args, m events.Message) bool {
	if !args.ExactMatch("type", string(m.Type)) || !args.ExactMatch("event", string(m.Action)) {
		return false
	}
	if !args.MatchKVList("label", m.Actor.Attributes) || !args.ExactMatch("image", m.Actor.Attributes["image"]) {
		return false
	}
	if args.Contains("container") {
		byIDOrName := args.ExactMatch("container", m.Actor.ID) || args.ExactMatch("container", m.Actor.Attributes["name"])
		if m.Type != events.ContainerEventType || !byIDOrName {
			return false
		}
	}
	if args.Contains("volume") && (m.Type != events.VolumeEventType || !args.ExactMatch("volume", m.Actor.ID)) {
		return false
	}
	return true
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
