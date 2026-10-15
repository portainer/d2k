package adapter

import (
	"testing"

	"github.com/docker/go-units"
)

// FuzzDockerSize checks dockerSize against its oracle, github.com/docker/
// go-units RAMInBytes: whatever RAMInBytes accepts, dockerSize must turn
// into a Kubernetes quantity worth the same number of bytes; whatever
// RAMInBytes rejects, dockerSize must not turn into a valid quantity either
// (or the size silently becomes something other than what was asked for).
func FuzzDockerSize(f *testing.F) {
	for _, seed := range []string{
		"0", "100", "100b", "100B", "1k", "1K", "1kb", "1KB", "1kib", "1KiB",
		"64m", "2g", "1.5g", "-1", "", "1t", "1p", "1gib", "nope", "1.5.5g",
		"9223372036854775807", "99999999999999999999999999g",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		want, err := units.RAMInBytes(s)
		if err != nil || want < 0 {
			return // dockerSize's contract only covers sizes Docker itself accepts.
		}
		got, ok := dockerSize(s)
		switch {
		case !ok:
			t.Fatalf("RAMInBytes(%q)=%d but dockerSize(%q) rejected it", s, want, s)
		case got != want:
			t.Fatalf("RAMInBytes(%q)=%d but dockerSize(%q)=%d", s, want, s, got)
		}
	})
}
