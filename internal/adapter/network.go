package adapter

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/internal/types"
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

// networkRecord is a network as it was created, recorded on the namespace.
type networkRecord struct {
	Name   string            `json:"name"`
	Driver string            `json:"driver,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	// Warnings name the options the network was created with that d2k
	// cannot honour, reported to the containers that join it.
	Warnings []string `json:"warnings,omitempty"`
}

func (r networkRecord) summary(namespace string) NetworkSummary {
	driver, scope := cmp.Or(r.Driver, "bridge"), "local"
	if driver == "overlay" {
		scope = "swarm"
	}
	return NetworkSummary{
		ID:     networkIDForName(r.Name, namespace),
		Name:   r.Name,
		Driver: driver,
		Scope:  scope,
		IPAM:   NetworkIPAM{Driver: "default", Config: []IPAMConfig{}},
		Labels: r.Labels,
	}
}

// CreateNetworkOptions mirrors docker network create flags.
type CreateNetworkOptions struct {
	Name           string
	Driver         string
	Labels         map[string]string
	Internal       bool
	EnableIPv6     bool
	DisableIPv4    bool
	IPAMConfigured bool
	Options        map[string]string
}

// CreateNetwork accepts a docker network create call and returns a synthetic
// network backed by the target namespace.
func (a *KubernetesDockerAdapter) CreateNetwork(ctx context.Context, opts CreateNetworkOptions) (*NetworkSummary, []string, error) {
	if opts.Name == "" {
		return nil, nil, errdefs.InvalidParameter(fmt.Errorf("network name is required"))
	}

	if opts.Driver == "macvlan" || opts.Driver == "ipvlan" {
		return nil, nil, errdefs.InvalidParameter(fmt.Errorf("network driver %q is not supported by d2k: Kubernetes pod networking has no Layer 2 attachment", opts.Driver))
	}

	var optionWarnings []string
	if opts.Internal {
		optionWarnings = append(optionWarnings, "internal is not enforced: containers can reach outside the cluster")
	}
	if opts.IPAMConfigured {
		optionWarnings = append(optionWarnings, "IPAM settings are ignored: the cluster assigns pod addresses")
	}
	if opts.EnableIPv6 {
		optionWarnings = append(optionWarnings, "IPv6 is not configured by d2k: pods get the cluster's address families")
	}
	if opts.DisableIPv4 {
		optionWarnings = append(optionWarnings, "IPv4 cannot be disabled: pods get the cluster's address families")
	}
	if len(opts.Options) > 0 {
		optionWarnings = append(optionWarnings, "driver options are ignored")
	}
	warnings := slices.Clone(optionWarnings)

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

	record := networkRecord{Name: opts.Name, Driver: opts.Driver, Labels: labels, Warnings: optionWarnings}
	if err := a.recordNetwork(ctx, record); err != nil {
		return nil, nil, err
	}
	summary := record.summary(a.namespace)
	return &summary, warnings, nil
}

// ListNetworks returns the synthetic network list.
func (a *KubernetesDockerAdapter) ListNetworks(ctx context.Context) ([]NetworkSummary, error) {
	records, err := a.networkRecords(ctx)
	if err != nil {
		return nil, err
	}

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

	// Include the networks created with CreateNetwork.
	for _, r := range records {
		networks = append(networks, r.summary(a.namespace))
	}

	// Add per-service networks for all swarm-managed deployments.
	// Portainer maps Spec.Networks[].Target against availableNetworks by ID —
	// if the ID isn't in this list the Networks panel shows empty. We generate
	// one synthetic overlay network per service, with the LB IP as the IPAM
	// subnet so Portainer displays it in the IP address column.
	deps, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelSwarmManagedBy + "=" + types.LabelSwarmManagedByValue,
	})
	if err == nil {
		for _, d := range deps.Items {
			netID := networkIDForName(d.Name, a.namespace)
			// Look up LB service IP for the subnet value.
			subnet := "10.0.0.0/8"
			lbSvc, svcErr := a.client.CoreV1().Services(a.namespace).Get(ctx, serviceName(d.Name)+"-lb", metav1.GetOptions{})
			if svcErr == nil {
				for _, ing := range lbSvc.Status.LoadBalancer.Ingress {
					if ing.IP != "" {
						subnet = ing.IP + "/32"
						break
					}
					if ing.Hostname != "" {
						subnet = ing.Hostname
						break
					}
				}
			}
			networks = append(networks, NetworkSummary{
				ID:         netID,
				Name:       d.Name,
				Driver:     "overlay",
				Scope:      "swarm",
				Attachable: true,
				IPAM:       NetworkIPAM{Driver: "default", Config: []IPAMConfig{{Subnet: subnet}}},
				Labels: map[string]string{
					types.LabelManagedBy:      types.LabelManagedByValue,
					types.LabelSwarmManagedBy: types.LabelSwarmManagedByValue,
				},
			})
		}
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

	// Networks created before they were recorded on the namespace are known
	// while containers have joined them. Other names do not exist, as compose
	// expects before creating.
	joined, err := a.joinedNetwork(ctx, nameOrID)
	if err != nil {
		return nil, err
	}
	if !joined {
		return nil, errdefs.NotFound(fmt.Errorf("network %s not found", nameOrID))
	}

	// Synthesise the network, with Compose labels if applicable.
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

// joinedNetwork reports whether a container joined the named network.
func (a *KubernetesDockerAdapter) joinedNetwork(ctx context.Context, name string) (bool, error) {
	deps, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return false, fmt.Errorf("unable to list deployments: %w", err)
	}
	for _, d := range deps.Items {
		if slices.Contains(containerNetworks(d), name) {
			return true, nil
		}
	}
	return false, nil
}

// runsOnNetwork reports whether a container that joined the named network
// runs, or is starting. As in Docker, where only those have an endpoint on
// it, created and stopped containers do not hold a network.
func (a *KubernetesDockerAdapter) runsOnNetwork(ctx context.Context, name string) (bool, error) {
	deps, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return false, fmt.Errorf("unable to list deployments: %w", err)
	}
	for _, d := range deps.Items {
		if !slices.Contains(containerNetworks(d), name) {
			continue
		}
		pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, podsOf(d.Name))
		if err != nil {
			return false, fmt.Errorf("unable to list pods: %w", err)
		}
		if state := containerState(d, pods.Items, time.Now()); state.Running || IsStarting(&state) {
			return true, nil
		}
	}
	return false, nil
}

// InspectNetworkDetail returns a full Docker network inspect response suitable
// for Portainer and other tooling that expects more fields than the list view.
//
// Per-service network IDs follow the pattern "d2k-<namespace>-<service>" and
// are used in Spec.Networks so Portainer can display the LB IP in the Networks
// panel (it reads IPAM.Config[0].Subnet from this response).
func (a *KubernetesDockerAdapter) InspectNetworkDetail(ctx context.Context, nameOrID string) (map[string]any, error) {
	// Check if this is a per-service network ID (d2k-<namespace>-<service>).
	// If so, look up the LB Service IP and return it as the IPAM subnet.
	prefix := "d2k-" + a.namespace + "-"
	if strings.HasPrefix(nameOrID, prefix) {
		serviceName := strings.TrimPrefix(nameOrID, prefix)
		if serviceName != "" && serviceName != a.namespace {
			return a.serviceNetworkDetail(ctx, nameOrID, serviceName)
		}
	}

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

// serviceNetworkDetail returns a synthetic network inspect response for a
// per-service network ID. It looks up the LoadBalancer Service for the given
// swarm service name and returns its external IP as the IPAM subnet, so
// Portainer displays it in the Networks panel IP address column.
func (a *KubernetesDockerAdapter) serviceNetworkDetail(ctx context.Context, networkID, svcName string) (map[string]any, error) {
	subnet := "10.0.0.0/8" // fallback if no LB IP assigned yet

	lbName := serviceName(svcName) + "-lb"
	svc, err := a.client.CoreV1().Services(a.namespace).Get(ctx, lbName, metav1.GetOptions{})
	if err == nil {
		for _, ing := range svc.Status.LoadBalancer.Ingress {
			if ing.IP != "" {
				subnet = ing.IP + "/32"
				break
			}
			if ing.Hostname != "" {
				subnet = ing.Hostname
				break
			}
		}
	}

	return map[string]any{
		"Name":       svcName,
		"Id":         networkID,
		"Created":    "2024-01-01T00:00:00.000000000Z",
		"Scope":      "swarm",
		"Driver":     "overlay",
		"EnableIPv6": false,
		"IPAM": map[string]any{
			"Driver":  "default",
			"Options": map[string]string{},
			"Config":  []map[string]any{{"Subnet": subnet}},
		},
		"Internal":   false,
		"Attachable": true,
		"Ingress":    false,
		"ConfigFrom": map[string]any{"Network": ""},
		"ConfigOnly": false,
		"Containers": map[string]any{},
		"Options":    map[string]string{},
		"Labels":     map[string]string{},
	}, nil
}

// RemoveNetwork removes a d2k-managed network by name or ID.
// Built-in networks (bridge, host, none) return an error matching Docker behaviour.
func (a *KubernetesDockerAdapter) RemoveNetwork(ctx context.Context, nameOrID string) error {
	switch nameOrID {
	case "bridge", "host", "none":
		return errdefs.Forbidden(fmt.Errorf("network %q is a pre-defined network and cannot be removed", nameOrID))
	}

	// The Docker CLI sends the ID (e.g. "d2k-d2k-example-app_backend")
	// rather than the name; resolve it back to the recorded name so
	// runsOnNetwork, which only knows names, can check it.
	name := nameOrID
	if records, err := a.networkRecords(ctx); err == nil {
		for _, r := range records {
			if r.Name == nameOrID || networkIDForName(r.Name, a.namespace) == nameOrID {
				name = r.Name
				break
			}
		}
	}
	if runs, err := a.runsOnNetwork(ctx, name); err != nil {
		return err
	} else if runs {
		return errdefs.Forbidden(fmt.Errorf("network %s has active endpoints", nameOrID))
	}

	// A network that is not found is not an error, as d2k has always answered.
	return a.removeNetworks(ctx, func(r networkRecord) bool {
		return r.Name == nameOrID || networkIDForName(r.Name, a.namespace) == nameOrID
	})
}

// networkIDForName produces a deterministic synthetic network ID from a name
// and namespace, giving Docker clients a stable ID to reference across calls.
// networkWarnings returns the warnings of the named networks' options, for
// a container that joins them.
func (a *KubernetesDockerAdapter) networkWarnings(ctx context.Context, names []string) []string {
	records, err := a.networkRecords(ctx)
	if err != nil {
		return []string{fmt.Sprintf("unable to read the options of networks %s: %s", strings.Join(names, ", "), err)}
	}
	var warnings []string
	for _, r := range records {
		if slices.Contains(names, r.Name) {
			for _, w := range r.Warnings {
				warnings = append(warnings, fmt.Sprintf("network %q: %s", r.Name, w))
			}
		}
	}
	return warnings
}

func networkIDForName(name, namespace string) string {
	return fmt.Sprintf("d2k-%s-%s", namespace, name)
}
