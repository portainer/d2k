// Package apispec holds the Docker Engine API routes d2k is measured against,
// generated from the pinned moby API specification in docker-v1.52.yaml
// (https://github.com/moby/moby/blob/master/api/swagger.yaml, v1.52.0, Apache-2.0).
package apispec

import (
	"cmp"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/pkg/httputils"
)

//go:generate go run ./gen docker-v1.52.yaml routes_gen.go

// MinVersion is the oldest API version d2k serves, as for Docker Engine 29.
// Version, the newest, comes from the pinned specification.
const MinVersion = "1.44"

// CheckVersion returns an error, as the Docker daemon words it, when a
// client's API version is outside [MinVersion, Version].
func CheckVersion(v string) error {
	switch {
	case compareVersions(v, MinVersion) < 0:
		return errdefs.InvalidParameter(fmt.Errorf("client version %s is too old. Minimum supported API version is %s, please upgrade your client to a newer version", v, MinVersion))
	case compareVersions(v, Version) > 0:
		return errdefs.InvalidParameter(fmt.Errorf("client version %s is too new. Maximum supported API version is %s", v, Version))
	}
	return nil
}

// compareVersions compares "major.minor" versions numerically: 1.5 < 1.44.
func compareVersions(a, b string) int {
	parse := func(v string) (int, int) {
		major, minor, _ := strings.Cut(v, ".")
		x, _ := strconv.Atoi(major)
		y, _ := strconv.Atoi(minor)
		return x, y
	}
	aMajor, aMinor := parse(a)
	bMajor, bMinor := parse(b)
	if c := cmp.Compare(aMajor, bMajor); c != 0 {
		return c
	}
	return cmp.Compare(aMinor, bMinor)
}

// Route is a Docker Engine API operation: a method and a path template such
// as "/containers/{id}/json".
type Route struct {
	Method string
	Path   string
}

var patterns = compile(routes)

// compile turns each path template into an anchored regexp. {name} may span
// several segments, since image and plugin names contain slashes
// ("library/nginx:latest"); other parameters match a single segment.
func compile(routes []Route) []*regexp.Regexp {
	params := regexp.MustCompile(`\{[^}]+\}`)
	compiled := make([]*regexp.Regexp, len(routes))
	for i, route := range routes {
		var expr strings.Builder
		last := 0
		for _, loc := range params.FindAllStringIndex(route.Path, -1) {
			expr.WriteString(regexp.QuoteMeta(route.Path[last:loc[0]]))
			if route.Path[loc[0]:loc[1]] == "{name}" {
				expr.WriteString(`.+`)
			} else {
				expr.WriteString(`[^/]+`)
			}
			last = loc[1]
		}
		expr.WriteString(regexp.QuoteMeta(route.Path[last:]))
		compiled[i] = regexp.MustCompile("^" + expr.String() + "$")
	}
	return compiled
}

// Match returns the spec route serving method and path, where path has no
// API version prefix. Literal routes win over templated ones, so
// "/containers/json" is not taken for "/containers/{id}".
func Match(method, path string) (Route, bool) {
	var match Route
	found := false
	for i, route := range routes {
		if route.Method != method || !patterns[i].MatchString(path) {
			continue
		}
		if !found || strings.Count(route.Path, "{") < strings.Count(match.Path, "{") {
			match, found = route, true
		}
	}
	return match, found
}

// NotRouted answers a request d2k has no handler for: a Docker-style 501 for
// an operation of the Docker Engine API, and a 404 for anything else.
func NotRouted(w http.ResponseWriter, r *http.Request) {
	if _, ok := Match(r.Method, r.URL.Path); ok {
		httputils.WriteError(w, http.StatusNotImplemented, fmt.Sprintf("%s %s is not supported by d2k", r.Method, r.URL.Path))
		return
	}
	httputils.WriteError(w, http.StatusNotFound, "page not found")
}
