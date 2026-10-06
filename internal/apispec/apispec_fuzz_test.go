package apispec

import (
	"strings"
	"testing"
)

// segments splits a path into its "/"-separated parts, as the swagger route
// templates (and the Docker Engine router) do, independently of Match's own
// regexp compilation.
func segments(p string) []string {
	return strings.Split(strings.TrimPrefix(p, "/"), "/")
}

// templateMatches reports whether path, segment by segment, fits route's
// template: a literal segment must appear verbatim, any other "{param}"
// matches exactly one segment, and "{name}" (image and plugin names, which
// contain slashes) may span one or more segments, wherever it falls in the
// template. Backtracking, since "{name}" is greedy only in that it may
// consume as many segments as the rest of the template leaves room for.
func templateMatches(route, path string) bool {
	if !strings.HasPrefix(path, "/") {
		return false
	}
	return matchSegments(segments(route), segments(path))
}

func matchSegments(rs, ps []string) bool {
	if len(rs) == 0 {
		return len(ps) == 0
	}
	r := rs[0]
	if r == "{name}" {
		// {name} is Go's regexp `.+`: it must consume at least one
		// character, so an empty segment alone does not satisfy it.
		for k := 1; k <= len(ps)-len(rs)+1; k++ {
			consumed := strings.Join(ps[:k], "/")
			// regexp's `.` (no (?s) flag) never matches a newline.
			if consumed == "" || strings.Contains(consumed, "\n") {
				continue
			}
			if matchSegments(rs[1:], ps[k:]) {
				return true
			}
		}
		return false
	}
	if len(ps) == 0 {
		return false
	}
	if strings.HasPrefix(r, "{") && strings.HasSuffix(r, "}") {
		// [^/]+ also requires at least one character.
		return ps[0] != "" && matchSegments(rs[1:], ps[1:])
	}
	return r == ps[0] && matchSegments(rs[1:], ps[1:])
}

// FuzzApispecMatch checks Match against an independent, segment-based
// reading of the same route templates it is generated from (the moby
// swagger spec's path syntax): Match must only ever return a route whose
// method and template genuinely fit the path, and among all fitting
// routes it must pick the one with the fewest template parameters (a
// literal route beats a templated one), exactly as the Engine's own router
// resolves "/containers/json" over "/containers/{id}".
func FuzzApispecMatch(f *testing.F) {
	for _, r := range routes {
		f.Add(r.Method, r.Path)
	}
	f.Add("GET", "/containers/json")
	f.Add("GET", "/containers/abc123/json")
	f.Add("GET", "/images/library/nginx:latest/json")
	f.Add("POST", "/containers/web/pause")
	f.Add("GET", "/../../etc/passwd")
	f.Add("GET", "")
	f.Add("get", "/containers/json")

	f.Fuzz(func(t *testing.T, method, path string) {
		got, ok := Match(method, path)
		if !ok {
			// Must not have missed a route that genuinely fits.
			for _, r := range routes {
				if r.Method == method && templateMatches(r.Path, path) {
					t.Fatalf("Match(%q, %q) = not found, but %q fits and is a real route", method, path, r.Path)
				}
			}
			return
		}
		if got.Method != method {
			t.Fatalf("Match(%q, %q) returned a route for method %q", method, path, got.Method)
		}
		if !templateMatches(got.Path, path) {
			t.Fatalf("Match(%q, %q) = %q, which does not actually fit", method, path, got.Path)
		}
		// Must be the most specific (fewest params) route that fits.
		gotParams := strings.Count(got.Path, "{")
		for _, r := range routes {
			if r.Method == method && templateMatches(r.Path, path) && strings.Count(r.Path, "{") < gotParams {
				t.Fatalf("Match(%q, %q) = %q (%d params) but %q (%d params) also fits and is more specific", method, path, got.Path, gotParams, r.Path, strings.Count(r.Path, "{"))
			}
		}
	})
}
