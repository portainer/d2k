package adapter

import (
	"context"
	"fmt"

	"github.com/portainer/d2k/internal/types"
)

const (
	// syntheticNetworkID is the fake network ID returned to Docker clients.
	// We use the namespace name as the network — all pods in the namespace share
	// the same flat network, so this is semantically accurate.
	syntheticNetworkDriver = "d2k"
)

// NetworkSummary is a Docker-compatible network entry.
type NetworkSummary struct {
	ID         string
	Name       string
	Driver     string
	Scope      string
	Internal   bool
	Labels     map[string]string
}

// CreateNetworkOptions mirrors docker network create flags.
type CreateNetworkOptions struct {
	Name   string
	Driver string
	Labels map[string]string
}

// CreateNetwork accepts a docker network create call and returns a synthetic
// network backed by the target namespace.
//
// Kubernetes networking within a namespace is flat — every Pod can reach every
// other Pod. There is no equivalent of Docker bridge isolation between networks
// within a namespace. d2k therefore accepts the call, records the network name
// as a label, and maps all containers to the same underlying namespace network.
//
// If the client creates multiple networks, they all resolve to the same flat
// namespace network. A warning is returned to make this behaviour visible.
func (a *KubernetesDockerAdapter) CreateNetwork(ctx context.Context, opts CreateNetworkOptions) (*NetworkSummary, []string, error) {
	if opts.Name == "" {
		return nil, nil, fmt.Errorf("network name is required")
	}

	var warnings []string

	// If the client is creating a network other than the default namespace network,
	// warn that isolation is not enforced.
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

	return summary, warnings, nil
}

// ListNetworks returns the synthetic network list.
// We always return at least the namespace default network plus any well-known names.
func (a *KubernetesDockerAdapter) ListNetworks(ctx context.Context) ([]NetworkSummary, error) {
	return []NetworkSummary{
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
		},
		{
			ID:     networkIDForName("host", a.namespace),
			Name:   "host",
			Driver: "host",
			Scope:  "host",
		},
		{
			ID:     networkIDForName("none", a.namespace),
			Name:   "none",
			Driver: "null",
			Scope:  "local",
		},
	}, nil
}

// InspectNetwork returns a synthetic network by name or ID.
func (a *KubernetesDockerAdapter) InspectNetwork(ctx context.Context, nameOrID string) (*NetworkSummary, error) {
	networks, _ := a.ListNetworks(ctx)
	for _, n := range networks {
		if n.Name == nameOrID || n.ID == nameOrID {
			return &n, nil
		}
	}

	// Unknown network names are treated as aliases for the namespace network.
	return &NetworkSummary{
		ID:     networkIDForName(nameOrID, a.namespace),
		Name:   nameOrID,
		Driver: syntheticNetworkDriver,
		Scope:  "local",
	}, nil
}

// RemoveNetwork is a no-op for d2k-managed networks since they are synthetic.
// Built-in networks (bridge, host, none) return an error matching Docker behaviour.
func (a *KubernetesDockerAdapter) RemoveNetwork(ctx context.Context, nameOrID string) error {
	switch nameOrID {
	case "bridge", "host", "none":
		return fmt.Errorf("network %q is a pre-defined network and cannot be removed", nameOrID)
	}

	// All other networks are synthetic — nothing to delete in Kubernetes.
	return nil
}

// networkIDForName produces a deterministic synthetic network ID from a name
// and namespace, giving Docker clients a stable ID to reference across calls.
func networkIDForName(name, namespace string) string {
	// Simple deterministic format: not a real UUID but stable and unique enough
	// for Docker client purposes within this d2k instance.
	return fmt.Sprintf("d2k-%s-%s", namespace, name)
}
