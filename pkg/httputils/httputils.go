package httputils

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/internal/filters"
)

// WriteJSON writes v as a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes a Docker-compatible error response body.
func WriteError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, map[string]string{"message": message})
}

// WriteErrorFrom writes err as a Docker error response, with the status the
// Docker daemon uses for its kind: errdefs types first, then Kubernetes API
// errors passed through from the cluster, and 500 for anything else.
func WriteErrorFrom(w http.ResponseWriter, err error) {
	WriteError(w, StatusFor(err), err.Error())
}

// StatusFor returns the HTTP status for err.
func StatusFor(err error) int {
	switch {
	case errdefs.IsInvalidParameter(err), apierrors.IsBadRequest(err), apierrors.IsInvalid(err):
		return http.StatusBadRequest
	case errdefs.IsForbidden(err), apierrors.IsForbidden(err):
		return http.StatusForbidden
	case errdefs.IsNotFound(err), apierrors.IsNotFound(err):
		return http.StatusNotFound
	case errdefs.IsConflict(err), apierrors.IsAlreadyExists(err), apierrors.IsConflict(err):
		return http.StatusConflict
	case errdefs.IsNotImplemented(err):
		return http.StatusNotImplemented
	case errdefs.IsUnavailable(err), apierrors.IsServiceUnavailable(err):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// ParseJSON decodes the request body into v.
func ParseJSON(r *http.Request, v any) error {
	defer func() { _ = r.Body.Close() }()
	return json.NewDecoder(r.Body).Decode(v)
}

// ParseFilters decodes the Docker `filters` query parameter and rejects keys
// outside accepted, as the Docker daemon does ("invalid filter").
func ParseFilters(r *http.Request, accepted map[string]bool) (filters.Args, error) {
	args, err := filters.FromJSON(r.URL.Query().Get("filters"))
	if err != nil {
		return args, err
	}
	return args, args.Validate(accepted)
}

// ParseUnixTime parses the since/until query values Docker uses: a Unix
// timestamp, optionally with a fractional part. Empty or "0" means unset.
func ParseUnixTime(v string) (time.Time, error) {
	if v == "" || v == "0" {
		return time.Time{}, nil
	}
	secs, frac, _ := strings.Cut(v, ".")
	s, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid timestamp %q: expected a Unix timestamp", v)
	}
	var nanos int64
	if frac != "" {
		frac = (frac + "000000000")[:9]
		if nanos, err = strconv.ParseInt(frac, 10, 64); err != nil {
			return time.Time{}, fmt.Errorf("invalid timestamp %q: expected a Unix timestamp", v)
		}
	}
	return time.Unix(s, nanos), nil
}
