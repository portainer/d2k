package adapter

import (
	"strconv"
	"strings"
	"testing"
)

// oracleParseNumericUser re-derives Docker's "uid" / "uid:gid" grammar from
// scratch (a decimal int64, optionally followed by ":" and a second decimal
// int64), independently of parseNumericUser's own use of strings.Cut, so a
// regression in the cut/parse logic (e.g. an off-by-one on the separator,
// or accepting a non-numeric uid) shows up here.
func oracleParseNumericUser(user string) (uid, gid int64, ok bool) {
	parts := strings.SplitN(user, ":", 2)
	u, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if len(parts) == 1 {
		return u, 0, true
	}
	g, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return u, g, true
}

// FuzzParseNumericUser checks parseNumericUser against that independent
// grammar for every input, not just the handful of cases translate_test.go
// exercises by hand.
func FuzzParseNumericUser(f *testing.F) {
	for _, s := range []string{"0", "1000", "1000:1000", "-1", "1000:", ":1000", "1000:abc", "abc", "", ":", "1000:1000:1000", "0x10", " 1000", "1000 ", "9223372036854775807", "9223372036854775808"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, user string) {
		wantUID, wantGID, wantOK := oracleParseNumericUser(user)
		gotUID, gotGID, gotOK := parseNumericUser(user)
		if gotOK != wantOK {
			t.Fatalf("parseNumericUser(%q) ok=%v, want %v", user, gotOK, wantOK)
		}
		if !wantOK {
			return
		}
		if gotUID == nil || *gotUID != wantUID {
			t.Fatalf("parseNumericUser(%q) uid=%v, want %d", user, gotUID, wantUID)
		}
		hasGroup := strings.Contains(user, ":")
		if !hasGroup {
			if gotGID != nil {
				t.Fatalf("parseNumericUser(%q) gid=%v, want nil (no ':')", user, *gotGID)
			}
			return
		}
		if gotGID == nil || *gotGID != wantGID {
			t.Fatalf("parseNumericUser(%q) gid=%v, want %d", user, gotGID, wantGID)
		}
	})
}
