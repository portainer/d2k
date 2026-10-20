package filters

import (
	"strings"
	"testing"
)

// oracleMatch, oracleExactMatch and oracleGetBoolOrDefault are copies of the
// same methods from moby's daemon/internal/filters/parse.go (the package
// this one was vendored from; it is an `internal` package there, so it
// cannot be imported directly) at the commit pinned for this overnight run.
// Any future change to filters.go that silently diverges from moby's
// semantics should show up here, rather than only being caught by hand.
func oracleExactMatch(fields map[string]map[string]bool, key, source string) bool {
	fieldValues, ok := fields[key]
	if !ok || len(fieldValues) == 0 {
		return true
	}
	return fieldValues[source]
}

func oracleGetBoolOrDefault(fields map[string]map[string]bool, key string, def bool) (bool, bool) {
	fieldValues, ok := fields[key]
	if !ok {
		return def, true
	}
	if len(fieldValues) == 0 {
		return def, false
	}
	isFalse := fieldValues["0"] || fieldValues["false"]
	isTrue := fieldValues["1"] || fieldValues["true"]
	if isFalse == isTrue {
		return def, false
	}
	return isTrue, true
}

// FuzzFiltersExactMatch feeds random key/value filters and a source string
// into Args built through the public API, then checks ExactMatch,
// GetBoolOrDefault and Contains against a direct read of the same map
// shape moby's own Args keeps.
func FuzzFiltersExactMatch(f *testing.F) {
	f.Add("status", "running", "exited", "running")
	f.Add("dangling", "true", "1", "true")
	f.Add("dangling", "true", "false", "x")
	f.Add("label", "a=b", "a=c", "a=b")
	f.Add("", "", "", "")

	f.Fuzz(func(t *testing.T, key, v1, v2, source string) {
		args := NewArgs(Arg(key, v1), Arg(key, v2))
		fields := map[string]map[string]bool{key: {v1: true, v2: true}}

		if got, want := args.ExactMatch(key, source), oracleExactMatch(fields, key, source); got != want {
			t.Fatalf("ExactMatch(%q,%q) with values {%q,%q} = %v, want %v", key, source, v1, v2, got, want)
		}
		if got := args.Contains(key); !got {
			t.Fatalf("Contains(%q) = false, want true: key was added via Arg", key)
		}
		wantBool, wantOK := oracleGetBoolOrDefault(fields, key, true)
		gotBool, err := args.GetBoolOrDefault(key, true)
		if gotOK := err == nil; gotOK != wantOK {
			t.Fatalf("GetBoolOrDefault(%q) err=%v, want ok=%v", key, err, wantOK)
		} else if wantOK && gotBool != wantBool {
			t.Fatalf("GetBoolOrDefault(%q) = %v, want %v", key, gotBool, wantBool)
		}
	})
}

// FuzzFiltersMatchKVList checks MatchKVList's "all pairs present" semantics
// against a from-scratch reading of the same rule moby documents: every
// key=value (or bare key) filter value must have a matching key in
// `sources`, with an equal value when one was given.
func FuzzFiltersMatchKVList(f *testing.F) {
	f.Add("network", "bridge=true", "bridge", "true")
	f.Add("network", "bridge", "bridge", "anything")
	f.Add("network", "bridge=true", "bridge", "false")
	f.Add("network", "bridge=true", "other", "true")

	f.Fuzz(func(t *testing.T, key, filterValue, sourceKey, sourceValue string) {
		args := NewArgs(Arg(key, filterValue))
		sources := map[string]string{}
		if sourceKey != "" {
			sources[sourceKey] = sourceValue
		}

		got := args.MatchKVList(key, sources)

		// Reference: the one configured value must be satisfiable by sources.
		k, v, hasValue := strings.Cut(filterValue, "=")
		sv, ok := sources[k]
		want := ok && (!hasValue || v == sv)
		if got != want {
			t.Fatalf("MatchKVList(%q, %v) with filter %q = %v, want %v", key, sources, filterValue, got, want)
		}
	})
}
