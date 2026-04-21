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

// NetworkIPAM is the IPAM configuration for a network.
type NetworkIPAM struct {
	Driver string       `json:"Driver"`
	Config []IPAMConfig `json:"Config"`
}

// IPAMConfig is a single IPAM address pool entry.
type IPAMConfig struct {
	Subnet  string `json:"Subnet,omitempty"`
	Gateway string `json:"Gateway,omitempty"`
}

// NetworkSummary is a Docker-compatible network entry.
type NetworkSummary struct {
	ID         string            `json:"Id"`
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Scope      string            `json:"Scope"`
	Internal   bool              `json:"Internal"`
	Attachable bool              `json:"Attachable"`
	IPAM       NetworkIPAM       `json:"IPAM"`
	Labels     map[string]string `json:"Labels"`
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
			"network %q created but Kubernetes namespace networking is flat - "+
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
		Driver: "overlay",
		Scope:  "swarm",
		IPAM:   NetworkIPAM{Driver: "default", Config: []IPAMConfig{}},
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
			ID:         networkIDForName(a.namespace, a.namespace),
			Name:       a.namespace,
			Driver:     "overlay",
			Scope:      "swarm",
			Attachable: true,
			// Portainer reads IPAM.Config[].Subnet to populate the IP address column
			// in the service Networks panel. Without a subnet entry the column is blank
			// even when VirtualIPs[].Addr is correctly populated.
			IPAM: NetworkIPAM{Driver: "default", Config: []IPAMConfig{
				{Subnet: "10.0.0.0/8"},
			}},
			Labels: map[string]string{
				types.LabelManagedBy: types.LabelManagedByValue,
			},
		},
		{
			ID:     networkIDForName("bridge", a.namespace),
			Name:   "bridge",
			Driver: "bridge",
			Scope:  "local",
			IPAM:   NetworkIPAM{Driver: "default", Config: []IPAMConfig{{Subnet: "172.17.0.0/16", Gateway: "172.17.0.1"}}},
			Labels: map[string]string{},
		},
		{
			ID:     networkIDForName("host", a.namespace),
			Name:   "host",
			Driver: "host",
			Scope:  "host",
			IPAM:   NetworkIPAM{Driver: "default", Config: []IPAMConfig{}},
			Labels: map[string]string{},
		},
		{
			ID:     networkIDForName("none", a.namespace),
			Name:   "none",
			Driver: "null",
			Scope:  "local",
			IPAM:   NetworkIPAM{Driver: "default", Config: []IPAMConfig{}},
			Labels: map[string]string{},
		},
		{
			ID:         networkIDForName("ingress", a.namespace),
			Name:       "ingress",
			Driver:     "overlay",
			Scope:      "swarm",
			Attachable: false,
			IPAM:       NetworkIPAM{Driver: "default", Config: []IPAMConfig{{Subnet: "10.0.0.0/24", Gateway: "10.0.0.1"}}},
			Labels:     map[string]string{"com.docker.network.driver.overlay.vxlanid_list": "4096"},
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
		Driver: "overlay",
		Scope:  "swarm",
		IPAM:   NetworkIPAM{Driver: "default", Config: []IPAMConfig{}},
		Labels: syntheticLabels,
	}, nil
}

// InspectNetworkDetail returns a full Docker network inspect response suitable
// for Portainer and other tooling that expects more fields than the list view.
func (a *KubernetesDockerAdapter) InspectNetworkDetail(ctx context.Context, nameOrID string) (map[string]any, error) {
	network, err := a.InspectNetwork(ctx, nameOrID)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"Name":       network.Name,
		"Id":         network.ID,
		"Created":    "2024-01-01T00:00:00.000000000Z",
		"Scope":      network.Scope,
		"Driver":     network.Driver,
		"EnableIPv6": false,
		"IPAM": map[string]any{
			"Driver":  network.IPAM.Driver,
			"Options": map[string]string{},
			"Config":  network.IPAM.Config,
		},
		"Internal":   network.Internal,
		"Attachable": network.Attachable,
		"Ingress":    network.Name == "ingress",
		"ConfigFrom": map[string]any{"Network": ""},
		"ConfigOnly": false,
		"Containers": map[string]any{},
		"Options":    map[string]string{},
		"Labels":     network.Labels,
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
