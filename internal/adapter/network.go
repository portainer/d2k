package adapter

import (
	"context"
	"fmt"
	"strings"

	"github.com/portainer/d2k/internal/types"
)

const (
	syntheticNetworkDriver = "d2k"
)

// NetworkSummary is a Docker-compatible network entry.
type NetworkSummary struct {
	ID       string            `json:"Id"`
	Name     string            `json:"Name"`
	Driver   string            `json:"Driver"`
	Scope    string            `json:"Scope"`
	Internal bool              `json:"Internal"`
	Labels   map[string]string `json:"Labels"`
}

// CreateNetworkOptions mirrors docker network create flags.
type CreateNetworkOptions struct {
	Name   string
	Driver string
	Labels map[string]string
}

// CreateNetwork accepts a docker network create call and returns a synthetic
// network backed by the target namespace.
func (a *KubernetesDockerAdapter) CreateNetwork(ctx context.Context, opts CreateNetworkOptions) (*NetworkSummary, []string, error) {
	if opts.Name == "" {
		return nil, nil, fmt.Errorf("network name is required")
	}

	var warnings []string

	if opts.Name != a.namespace && opts.Name != "bridge" && opts.Name != "host" {
		warnings = append(warnings, fmt.Sprintf(
			"network %q created but Kubernetes namespace networking is flat — "+
				"all containers share the same network regardless of which network they are assigned to",
			opts.Name,
		))
	}

	labels := map[string]string{
		types.LabelManagedBy:    types.LabelManagedByValue,
		types.LabelWorkloadName: opts.Name,
	}
	// Synthesise Compose labels if the name matches <project>_<network> pattern.
	if idx := strings.LastIndex(opts.Name, "_"); idx != -1 {
		labels["com.docker.compose.network"] = opts.Name[idx+1:]
		labels["com.docker.compose.project"] = opts.Name[:idx]
	}
	for k, v := range opts.Labels {
		labels[k] = v
	}

	summary := &NetworkSummary{
		ID:     networkIDForName(opts.Name, a.namespace),
		Name:   opts.Name,
		Driver: syntheticNetworkDriver,
		Scope:  "local",
		Labels: labels,
	}

	a.networksMu.Lock()
	a.networks[opts.Name] = summary
	a.networksMu.Unlock()

	return summary, warnings, nil
}

// ListNetworks returns the synthetic network list.
func (a *KubernetesDockerAdapter) ListNetworks(ctx context.Context) ([]NetworkSummary, error) {
	a.networksMu.RLock()
	defer a.networksMu.RUnlock()

	networks := []NetworkSummary{
		{
			ID:     networkIDForName(a.namespace, a.namespace),
			Name:   a.namespace,
			Driver: syntheticNetworkDriver,
			Scope:  "local",
			Labels: map[string]string{
				types.LabelManagedBy: types.LabelManagedByValue,
			},
		},
		{
			ID:     networkIDForName("bridge", a.namespace),
			Name:   "bridge",
			Driver: "bridge",
			Scope:  "local",
			Labels: map[string]string{},
		},
		{
			ID:     networkIDForName("host", a.namespace),
			Name:   "host",
			Driver: "host",
			Scope:  "host",
			Labels: map[string]string{},
		},
		{
			ID:     networkIDForName("none", a.namespace),
			Name:   "none",
			Driver: "null",
			Scope:  "local",
			Labels: map[string]string{},
		},
	}

	// Include any networks created via CreateNetwork.
	for _, n := range a.networks {
		networks = append(networks, *n)
	}

	return networks, nil
}

// InspectNetwork returns a synthetic network by name or ID.
func (a *KubernetesDockerAdapter) InspectNetwork(ctx context.Context, nameOrID string) (*NetworkSummary, error) {
	networks, _ := a.ListNetworks(ctx)
	for _, n := range networks {
		if n.Name == nameOrID || n.ID == nameOrID {
			return &n, nil
		}
	}

	// Synthesise a response for unknown networks, with Compose labels if applicable.
	syntheticLabels := map[string]string{}
	if idx := strings.LastIndex(nameOrID, "_"); idx != -1 {
		syntheticLabels["com.docker.compose.network"] = nameOrID[idx+1:]
		syntheticLabels["com.docker.compose.project"] = nameOrID[:idx]
	}

	return &NetworkSummary{
		ID:     networkIDForName(nameOrID, a.namespace),
		Name:   nameOrID,
		Driver: syntheticNetworkDriver,
		Scope:  "local",
		Labels: syntheticLabels,
	}, nil
}

// RemoveNetwork is a no-op for d2k-managed networks since they are synthetic.
// Built-in networks (bridge, host, none) return an error matching Docker behaviour.
func (a *KubernetesDockerAdapter) RemoveNetwork(ctx context.Context, nameOrID string) error {
	switch nameOrID {
	case "bridge", "host", "none":
		return fmt.Errorf("network %q is a pre-defined network and cannot be removed", nameOrID)
	}

	a.networksMu.Lock()
	delete(a.networks, nameOrID)
	a.networksMu.Unlock()

	return nil
}

// networkIDForName produces a deterministic synthetic network ID from a name
// and namespace, giving Docker clients a stable ID to reference across calls.
func networkIDForName(name, namespace string) string {
	return fmt.Sprintf("d2k-%s-%s", namespace, name)
}
