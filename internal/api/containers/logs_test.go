package containers

import (
	"strings"
	"testing"
	"time"
)

func TestCopyLogLines(t *testing.T) {
	src := "2026-10-05T12:00:01.000000000Z one\n" +
		"2026-10-05T12:00:02.000000000Z two\n" +
		"2026-10-05T12:00:03.000000000Z three\n" +
		"2026-10-05T12:00:04.000000000Z no newline"
	collect := func(until time.Time, keepTimestamps bool) []string {
		var lines []string
		err := copyLogLines(strings.NewReader(src), until, keepTimestamps, func(line []byte) error {
			lines = append(lines, string(line))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return lines
	}

	all := collect(time.Time{}, true)
	if len(all) != 4 || all[0] != "2026-10-05T12:00:01.000000000Z one\n" || all[3] != "2026-10-05T12:00:04.000000000Z no newline" {
		t.Fatalf("with timestamps = %q", all)
	}
	stripped := collect(time.Time{}, false)
	if stripped[1] != "two\n" {
		t.Fatalf("without timestamps = %q", stripped)
	}
	// until is inclusive in Docker; later lines end the stream.
	if got := collect(time.Date(2026, 10, 5, 12, 0, 2, 0, time.UTC), false); len(got) != 2 || got[1] != "two\n" {
		t.Fatalf("until 12:00:02 = %q", got)
	}
}

func TestParseLogTime(t *testing.T) {
	for in, want := range map[string]time.Time{
		"":                     {},
		"0":                    {},
		"1759665601":           time.Unix(1759665601, 0),
		"1759665601.250000000": time.Unix(1759665601, 250000000),
	} {
		got, err := parseLogTime(in)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseLogTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseLogTime("yesterday"); err == nil {
		t.Error("parseLogTime(yesterday) succeeded")
	}
}
