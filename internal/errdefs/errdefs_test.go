package errdefs

import (
	"errors"
	"fmt"
	"testing"

	"github.com/portainer/d2k/internal/filters"
)

func TestKinds(t *testing.T) {
	base := errors.New("container web not found")
	err := fmt.Errorf("inspect: %w", NotFound(base))
	if !IsNotFound(err) || IsConflict(err) {
		t.Errorf("wrapped NotFound: IsNotFound=%v IsConflict=%v", IsNotFound(err), IsConflict(err))
	}
	if !errors.Is(err, base) || err.Error() != "inspect: container web not found" {
		t.Errorf("NotFound changed the error chain or message: %v", err)
	}
	if NotFound(nil) != nil {
		t.Error("NotFound(nil) != nil")
	}
	for name, tt := range map[string]struct {
		err error
		is  func(error) bool
	}{
		"invalid parameter": {InvalidParameter(base), IsInvalidParameter},
		"conflict":          {Conflict(base), IsConflict},
		"forbidden":         {Forbidden(base), IsForbidden},
		"not implemented":   {NotImplemented(base), IsNotImplemented},
		"unavailable":       {Unavailable(base), IsUnavailable},
	} {
		if !tt.is(tt.err) || IsNotFound(tt.err) {
			t.Errorf("%s: kind not detected or confused with NotFound", name)
		}
	}
}

func TestInvalidFilterIsInvalidParameter(t *testing.T) {
	_, err := filters.FromJSON("{bad")
	if !IsInvalidParameter(err) {
		t.Errorf("filters.FromJSON error %v is not an InvalidParameter", err)
	}
}
