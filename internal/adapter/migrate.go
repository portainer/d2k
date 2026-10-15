package adapter

import (
	"context"
	"errors"
)

// MigrateLegacyObjects records, on the containers and volumes an earlier
// d2k release created, what the current release reads.
func (a *KubernetesDockerAdapter) MigrateLegacyObjects(ctx context.Context, swarmMode bool) error {
	return errors.New("not implemented")
}
