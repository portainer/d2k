package system

import (
	"slices"
	"strings"
	"testing"

	"github.com/portainer/d2k/internal/adapter"
)

func TestPodSecurityInfo(t *testing.T) {
	for _, tt := range []struct {
		level       string
		wantOption  string
		wantWarning string
	}{
		{adapter.PodSecurityPrivileged, "name=pod-security,profile=privileged", ""},
		{adapter.PodSecurityBaseline, "name=pod-security,profile=baseline", "refuses --privileged"},
		{adapter.PodSecurityRestricted, "name=pod-security,profile=restricted", "--cap-drop ALL"},
		{adapter.PodSecurityUnknown, "", ""},
	} {
		t.Run(tt.level, func(t *testing.T) {
			options, warnings := podSecurityInfo("d2k", tt.level)

			// Every pod d2k creates uses Docker's default seccomp profile.
			if !slices.Contains(options, "name=seccomp,profile=builtin") {
				t.Errorf("security options = %q, want Docker's default seccomp profile", options)
			}
			if hasLevel := slices.ContainsFunc(options, func(o string) bool { return strings.HasPrefix(o, "name=pod-security") }); tt.wantOption == "" && hasLevel || tt.wantOption != "" && !slices.Contains(options, tt.wantOption) {
				t.Errorf("security options = %q, want %q", options, tt.wantOption)
			}
			switch {
			case tt.wantWarning == "" && len(warnings) != 0:
				t.Errorf("warnings = %q, want none", warnings)
			case tt.wantWarning != "" && (len(warnings) != 1 || !strings.HasPrefix(warnings[0], "WARNING: namespace d2k enforces") || !strings.Contains(warnings[0], tt.wantWarning)):
				t.Errorf("warnings = %q, want one naming %q", warnings, tt.wantWarning)
			}
		})
	}
}
