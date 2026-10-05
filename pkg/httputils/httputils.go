package httputils

import (
	"encoding/json"
	"net/http"

	"github.com/docker/docker/api/types/filters"
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

// ParseJSON decodes the request body into v.
func ParseJSON(r *http.Request, v any) error {
	defer r.Body.Close()
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
