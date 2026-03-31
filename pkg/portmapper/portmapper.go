// Package portmapper determines the Kubernetes Service type to create based on
// Docker port mapping arguments, implementing the following rules:
//
//   - No -p flags at all: no Service created
//   - -P (publish all exposed ports): NodePort Service
//   - -p <host>:<container> (explicit mapping): LoadBalancer Service
//
// If a host IP is included in an explicit mapping (e.g. -p 127.0.0.1:8080:80),
// the host IP component is ignored and a LoadBalancer is still created. A warning
// is logged since host-IP binding has no equivalent in Kubernetes.
package portmapper

import (
	"fmt"
	"strconv"
	"strings"
)

// MappingKind describes the Service type d2k should create.
type MappingKind int

const (
	// NoService means no port was requested; no Service will be created.
	NoService MappingKind = iota
	// NodePortService means -P was used; a NodePort Service will be created.
	NodePortService
	// LoadBalancerService means explicit -p mappings were given; a LoadBalancer Service will be created.
	LoadBalancerService
)

// PortMapping represents a single resolved host→container port pair.
type PortMapping struct {
	// HostPort is the external port requested by the Docker client.
	// For NodePort Services this is advisory only (Kubernetes assigns the actual NodePort).
	HostPort int
	// ContainerPort is the port the container listens on.
	ContainerPort int
	// Protocol is "TCP" or "UDP".
	Protocol string
}

// Request captures all port-related flags from a docker run call.
type Request struct {
	// PublishAll corresponds to -P / --publish-all.
	PublishAll bool
	// ExposedPorts are the ports declared in the image (used when PublishAll is true).
	// Keys are "<port>/<proto>" strings, e.g. "80/tcp".
	ExposedPorts map[string]struct{}
	// PortBindings are explicit -p mappings.
	// Each entry is a raw string as passed by the Docker client, e.g. "8080:80", "127.0.0.1:443:443/tcp".
	PortBindings []string
}

// Resolve returns the MappingKind and the resolved port list for the given request.
// Warnings contains any non-fatal advisory messages (e.g. ignored host IP).
func Resolve(req Request) (kind MappingKind, mappings []PortMapping, warnings []string, err error) {
	if req.PublishAll {
		// -P: NodePort for every exposed port declared in the image.
		for portProto := range req.ExposedPorts {
			pm, e := parsePortProto(portProto)
			if e != nil {
				return NoService, nil, warnings, fmt.Errorf("invalid exposed port %q: %w", portProto, e)
			}
			// For NodePort, HostPort is 0 — Kubernetes assigns one.
			pm.HostPort = 0
			mappings = append(mappings, pm)
		}
		return NodePortService, mappings, warnings, nil
	}

	if len(req.PortBindings) == 0 {
		return NoService, nil, warnings, nil
	}

	// Explicit -p mappings → LoadBalancer.
	for _, raw := range req.PortBindings {
		pm, warn, e := parseExplicitBinding(raw)
		if e != nil {
			return NoService, nil, warnings, fmt.Errorf("invalid port binding %q: %w", raw, e)
		}
		if warn != "" {
			warnings = append(warnings, warn)
		}
		mappings = append(mappings, pm)
	}

	return LoadBalancerService, mappings, warnings, nil
}

// parsePortProto parses a "<port>/<proto>" string (e.g. "80/tcp") into a PortMapping.
// HostPort is left as zero; callers set it as appropriate.
func parsePortProto(s string) (PortMapping, error) {
	parts := strings.SplitN(s, "/", 2)
	port, err := strconv.Atoi(parts[0])
	if err != nil {
		return PortMapping{}, fmt.Errorf("non-numeric port: %w", err)
	}

	proto := "TCP"
	if len(parts) == 2 {
		proto = strings.ToUpper(parts[1])
	}

	return PortMapping{ContainerPort: port, Protocol: proto}, nil
}

// parseExplicitBinding parses a raw Docker -p binding string into a PortMapping.
// Accepted formats:
//
//	<hostPort>:<containerPort>
//	<hostPort>:<containerPort>/<proto>
//	<hostIP>:<hostPort>:<containerPort>
//	<hostIP>:<hostPort>:<containerPort>/<proto>
func parseExplicitBinding(raw string) (pm PortMapping, warning string, err error) {
	// Strip optional protocol suffix.
	proto := "TCP"
	if idx := strings.LastIndex(raw, "/"); idx != -1 {
		proto = strings.ToUpper(raw[idx+1:])
		raw = raw[:idx]
	}
	pm.Protocol = proto

	parts := strings.Split(raw, ":")

	switch len(parts) {
	case 2:
		// <hostPort>:<containerPort>
		pm.HostPort, err = strconv.Atoi(parts[0])
		if err != nil {
			return pm, "", fmt.Errorf("invalid host port %q: %w", parts[0], err)
		}
		pm.ContainerPort, err = strconv.Atoi(parts[1])
		if err != nil {
			return pm, "", fmt.Errorf("invalid container port %q: %w", parts[1], err)
		}

	case 3:
		// <hostIP>:<hostPort>:<containerPort> — host IP is ignored.
		warning = fmt.Sprintf("host IP %q in port binding %q has no Kubernetes equivalent and will be ignored; a LoadBalancer Service will be created without host IP restriction", parts[0], raw)
		pm.HostPort, err = strconv.Atoi(parts[1])
		if err != nil {
			return pm, warning, fmt.Errorf("invalid host port %q: %w", parts[1], err)
		}
		pm.ContainerPort, err = strconv.Atoi(parts[2])
		if err != nil {
			return pm, warning, fmt.Errorf("invalid container port %q: %w", parts[2], err)
		}

	default:
		return pm, "", fmt.Errorf("unrecognised port binding format %q", raw)
	}

	return pm, warning, nil
}
