package adapter

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// configHandled and hostConfigHandled list the create request settings that
// RunOptionsFromRequest translates or that have no effect on a container in
// Kubernetes (attach flags, console size). Any other setting a client sets is
// reported back as a warning instead of being dropped silently.
var (
	configHandled = map[string]bool{
		"AttachStdin": true, "AttachStdout": true, "AttachStderr": true, "ArgsEscaped": true,
		"Image": true, "Entrypoint": true, "Cmd": true, "Env": true, "Labels": true, "ExposedPorts": true,
		"User": true, "WorkingDir": true, "Tty": true, "OpenStdin": true, "StdinOnce": true, "StopTimeout": true,
		"Healthcheck": true, "Hostname": true,
	}
	hostConfigHandled = map[string]bool{
		"Binds": true, "Mounts": true, "NetworkMode": true, "PortBindings": true, "PublishAllPorts": true,
		"RestartPolicy": true, "ConsoleSize": true, "LogConfig": true, "ExtraHosts": true,
		"CapAdd": true, "CapDrop": true, "Privileged": true, "ReadonlyRootfs": true,
		"Memory": true, "MemoryReservation": true, "NanoCPUs": true,
		"CPUShares": true, "CPUQuota": true, "CPUPeriod": true, "DeviceRequests": true,
		"DNS": true, "DNSSearch": true, "DNSOptions": true, "Sysctls": true, "ShmSize": true, "Tmpfs": true,
		"SecurityOpt": true, "GroupAdd": true, "Annotations": true, "AutoRemove": true,
	}
	// Links are reported with HostConfig.Links, where compose also sets them.
	endpointHandled = map[string]bool{"Aliases": true, "Links": true}
)

// RunOptionsFromRequest translates a Docker container create request into
// RunOptions. The warnings name every setting d2k cannot honour.
func RunOptionsFromRequest(name string, req container.CreateRequest) (RunOptions, []string) {
	cfg := container.Config{}
	if req.Config != nil {
		cfg = *req.Config
	}
	host := container.HostConfig{}
	if req.HostConfig != nil {
		host = *req.HostConfig
	}

	opts := RunOptions{
		Name:       name,
		Image:      cfg.Image,
		Entrypoint: cfg.Entrypoint,
		Cmd:        cfg.Cmd,
		Env:        cfg.Env,
		Labels:     cfg.Labels,
		PublishAll: host.PublishAllPorts,
		Binds:      host.Binds,
		WorkingDir: cfg.WorkingDir,
		Tty:        cfg.Tty,
		Stdin:      cfg.OpenStdin,
		StdinOnce:  cfg.StdinOnce,
		CapAdd:     host.CapAdd,
		CapDrop:    host.CapDrop,
		Privileged: host.Privileged,
		ReadOnly:   host.ReadonlyRootfs,
	}
	warnings := ignoredSettings(cfg, host)

	if len(cfg.ExposedPorts) > 0 {
		opts.ExposedPorts = map[string]struct{}{}
		for p := range cfg.ExposedPorts {
			opts.ExposedPorts[p.String()] = struct{}{}
		}
	}
	for p, bindings := range host.PortBindings {
		for _, b := range bindings {
			if b.HostIP.IsValid() && !b.HostIP.IsUnspecified() {
				opts.PortBindings = append(opts.PortBindings, fmt.Sprintf("%s:%s:%d", b.HostIP, b.HostPort, p.Num()))
			} else {
				opts.PortBindings = append(opts.PortBindings, fmt.Sprintf("%s:%d", b.HostPort, p.Num()))
			}
		}
	}

	if cfg.User != "" {
		uid, gid, ok := parseNumericUser(cfg.User)
		if ok {
			opts.RunAsUser, opts.RunAsGroup = uid, gid
		} else {
			warnings = append(warnings, fmt.Sprintf("user %q ignored: Kubernetes needs a numeric UID (and GID)", cfg.User))
		}
	}
	if cfg.StopTimeout != nil && *cfg.StopTimeout >= 0 {
		grace := int64(*cfg.StopTimeout)
		opts.StopTimeout = &grace
	}

	var mountWarnings []string
	opts.Binds, mountWarnings = append(slices.Clone(opts.Binds), volumeMountBinds(host.Mounts)...), mountWarningsFor(host.Mounts)
	warnings = append(warnings, mountWarnings...)

	var hostWarnings []string
	opts.HostAliases, hostWarnings = hostAliases(host.ExtraHosts)
	warnings = append(warnings, hostWarnings...)

	var healthWarnings []string
	opts.Healthcheck, healthWarnings = healthProbe(cfg.Healthcheck)
	warnings = append(warnings, healthWarnings...)

	native, nativeWarnings := nativeSettings(cfg, host)
	opts.Hostname, opts.DNSSearch, opts.DNSOptions, opts.Sysctls = native.Hostname, native.DNSSearch, native.DNSOptions, native.Sysctls
	opts.Tmpfs, opts.NoNewPrivileges, opts.Seccomp, opts.AppArmor, opts.SELinux = native.Tmpfs, native.NoNewPrivileges, native.Seccomp, native.AppArmor, native.SELinux
	opts.SupplementalGroups, opts.Annotations, opts.VolumeSubpaths = native.SupplementalGroups, native.Annotations, native.VolumeSubpaths
	warnings = append(warnings, nativeWarnings...)

	opts.Resources = resources(host.Resources)
	opts.GPUCount = gpuCount(host.DeviceRequests)

	opts.RestartPolicy = string(host.RestartPolicy.Name)
	if opts.RestartPolicy == "" {
		opts.RestartPolicy = string(container.RestartPolicyDisabled)
	}
	opts.AutoRemove = host.AutoRemove
	if n := host.RestartPolicy.MaximumRetryCount; n > 0 {
		warnings = append(warnings, fmt.Sprintf("on-failure maximum retry count %d ignored: Kubernetes retries without limit", n))
	}
	if t := host.LogConfig.Type; t != "" && t != "json-file" {
		warnings = append(warnings, fmt.Sprintf("log driver %q is not supported by d2k: logs are read from Kubernetes", t))
	}
	if len(host.LogConfig.Config) > 0 {
		warnings = append(warnings, fmt.Sprintf("log options %s ignored: the kubelet rotates container logs", strings.Join(slices.Sorted(maps.Keys(host.LogConfig.Config)), ", ")))
	}

	switch mode := string(host.NetworkMode); {
	case mode == "host" || mode == "none" || strings.HasPrefix(mode, "container:"):
		warnings = append(warnings, fmt.Sprintf("network mode %q is not supported by d2k: the container joins the namespace network", mode))
	case mode != "" && mode != "default" && mode != "bridge":
		opts.Networks = append(opts.Networks, mode)
	}
	if req.NetworkingConfig != nil {
		for _, network := range slices.Sorted(maps.Keys(req.NetworkingConfig.EndpointsConfig)) {
			endpoint := req.NetworkingConfig.EndpointsConfig[network]
			if endpoint != nil {
				opts.Aliases = append(opts.Aliases, endpoint.Aliases...)
				warnings = append(warnings, unhandledFields("NetworkingConfig.EndpointsConfig."+network+".", reflect.ValueOf(*endpoint), endpointHandled)...)
			}
			if !slices.Contains(opts.Networks, network) {
				opts.Networks = append(opts.Networks, network)
			}
		}
	}
	return opts, warnings
}

// ignoredSettings names the settings the client set that d2k does not
// translate. MemorySwappiness -1 is the Docker CLI's default, not a choice.
func ignoredSettings(cfg container.Config, host container.HostConfig) []string {
	if host.MemorySwappiness != nil && *host.MemorySwappiness == -1 {
		host.MemorySwappiness = nil
	}
	return append(unhandledFields("", reflect.ValueOf(cfg), configHandled), unhandledFields("HostConfig.", reflect.ValueOf(host), hostConfigHandled)...)
}

// unhandledFields names the fields of struct v that are set and not handled.
func unhandledFields(prefix string, v reflect.Value, handled map[string]bool) []string {
	var ignored []string
	for i := range v.NumField() {
		field, value := v.Type().Field(i), v.Field(i)
		if field.Anonymous {
			ignored = append(ignored, unhandledFields(prefix, value, handled)...)
			continue
		}
		if !field.IsExported() || handled[field.Name] || isUnset(value) {
			continue
		}
		ignored = append(ignored, fmt.Sprintf("%s%s is not supported by d2k and was ignored", prefix, jsonName(field)))
	}
	return ignored
}

// isUnset reports whether a client left a setting at its default. Clients
// send empty lists and maps, and pointers to zero values, for settings they
// do not use.
func isUnset(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Slice, reflect.Map:
		return v.Len() == 0
	case reflect.Pointer:
		return v.IsNil() || isUnset(v.Elem())
	case reflect.Struct:
		for i := range v.NumField() {
			if !isUnset(v.Field(i)) {
				return false
			}
		}
		return true
	default:
		return v.IsZero()
	}
}

// jsonName returns the name a field has in the API, which clients know.
func jsonName(f reflect.StructField) string {
	if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
		return name
	}
	return f.Name
}

// parseNumericUser parses Docker's "uid" or "uid:gid".
func parseNumericUser(user string) (uid, gid *int64, ok bool) {
	u, g, hasGroup := strings.Cut(user, ":")
	id, err := strconv.ParseInt(u, 10, 64)
	if err != nil {
		return nil, nil, false
	}
	uid = &id
	if hasGroup {
		group, err := strconv.ParseInt(g, 10, 64)
		if err != nil {
			return nil, nil, false
		}
		gid = &group
	}
	return uid, gid, true
}

// volumeMountBinds turns --mount entries of type volume into binds, which
// namedVolumeMounts translates.
func volumeMountBinds(mounts []mount.Mount) []string {
	var binds []string
	for _, m := range mounts {
		if m.Type != mount.TypeVolume {
			continue
		}
		bind := m.Source + ":" + m.Target
		if m.ReadOnly {
			bind += ":ro"
		}
		binds = append(binds, bind)
	}
	return binds
}

func mountWarningsFor(mounts []mount.Mount) []string {
	var warnings []string
	for _, m := range mounts {
		if m.Type != mount.TypeVolume && m.Type != mount.TypeTmpfs {
			warnings = append(warnings, fmt.Sprintf("%s mount at %q ignored: only named volumes and tmpfs are supported by d2k", m.Type, m.Target))
			continue
		}
		if o := m.VolumeOptions; m.Type == mount.TypeVolume && o != nil && o.DriverConfig != nil && o.DriverConfig.Name != "" && o.DriverConfig.Name != "local" {
			warnings = append(warnings, fmt.Sprintf("volume driver %q ignored at %q: d2k volumes are PersistentVolumeClaims", o.DriverConfig.Name, m.Target))
		}
	}
	return warnings
}

// hostAliases translates --add-host "name:ip" entries.
func hostAliases(extraHosts []string) ([]corev1.HostAlias, []string) {
	var aliases []corev1.HostAlias
	var warnings []string
	for _, entry := range extraHosts {
		host, ip, ok := strings.Cut(entry, ":")
		if !ok {
			host, ip, ok = strings.Cut(entry, "=")
		}
		ip = strings.Trim(ip, "[]")
		if !ok || ip == "host-gateway" {
			warnings = append(warnings, fmt.Sprintf("extra host %q ignored: only name:IP entries are supported by d2k", entry))
			continue
		}
		if i := slices.IndexFunc(aliases, func(a corev1.HostAlias) bool { return a.IP == ip }); i >= 0 {
			aliases[i].Hostnames = append(aliases[i].Hostnames, host)
			continue
		}
		aliases = append(aliases, corev1.HostAlias{IP: ip, Hostnames: []string{host}})
	}
	return aliases, warnings
}

// resources translates Docker's memory and CPU settings: limits from
// Memory and NanoCPUs (or CPUQuota/CPUPeriod), requests from
// MemoryReservation and CPUShares (1024 shares = 1 CPU).
func resources(r container.Resources) corev1.ResourceRequirements {
	limits, requests := corev1.ResourceList{}, corev1.ResourceList{}
	if r.Memory > 0 {
		limits[corev1.ResourceMemory] = *resource.NewQuantity(r.Memory, resource.BinarySI)
	}
	if r.MemoryReservation > 0 {
		requests[corev1.ResourceMemory] = *resource.NewQuantity(r.MemoryReservation, resource.BinarySI)
	}
	switch {
	case r.NanoCPUs > 0:
		limits[corev1.ResourceCPU] = *resource.NewMilliQuantity(r.NanoCPUs/1_000_000, resource.DecimalSI)
	case r.CPUQuota > 0 && r.CPUPeriod > 0:
		limits[corev1.ResourceCPU] = *resource.NewMilliQuantity(r.CPUQuota*1000/r.CPUPeriod, resource.DecimalSI)
	}
	if r.CPUShares > 0 {
		requests[corev1.ResourceCPU] = *resource.NewMilliQuantity(r.CPUShares*1000/1024, resource.DecimalSI)
	}
	out := corev1.ResourceRequirements{}
	if len(limits) > 0 {
		out.Limits = limits
	}
	if len(requests) > 0 {
		out.Requests = requests
	}
	return out
}

// Docker's healthcheck defaults.
const (
	defaultHealthInterval = 30 * time.Second
	defaultHealthTimeout  = 30 * time.Second
	defaultHealthRetries  = 3
)

// healthProbe translates a Docker healthcheck into a readiness probe, whose
// result becomes the container's health. d2k's Services publish unready
// pods, so as in Docker an unhealthy container keeps its traffic.
func healthProbe(h *container.HealthConfig) (*corev1.Probe, []string) {
	if h == nil {
		return nil, nil
	}
	var command []string
	switch {
	case len(h.Test) == 0:
		return nil, []string{"healthcheck inherited from the image ignored: d2k does not read image configurations"}
	case h.Test[0] == "NONE":
		return nil, nil
	case h.Test[0] == "CMD" && len(h.Test) > 1:
		command = h.Test[1:]
	case h.Test[0] == "CMD-SHELL" && len(h.Test) == 2:
		command = []string{"/bin/sh", "-c", h.Test[1]}
	default:
		return nil, []string{fmt.Sprintf("healthcheck %q ignored: expected CMD, CMD-SHELL or NONE", h.Test)}
	}

	seconds := func(d, fallback time.Duration) int32 {
		if d <= 0 {
			d = fallback
		}
		return int32(max(1, d/time.Second))
	}
	retries := h.Retries
	if retries <= 0 {
		retries = defaultHealthRetries
	}
	var warnings []string
	if h.StartInterval > 0 {
		warnings = append(warnings, "healthcheck start interval ignored: Kubernetes probes use one period")
	}
	return &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: command}},
		PeriodSeconds:       seconds(h.Interval, defaultHealthInterval),
		TimeoutSeconds:      seconds(h.Timeout, defaultHealthTimeout),
		FailureThreshold:    int32(retries),
		SuccessThreshold:    1,
		InitialDelaySeconds: int32(h.StartPeriod / time.Second),
	}, warnings
}

// gpuCount returns the GPUs requested with --gpus. Docker sends Count=-1 for
// "all"; Kubernetes device plugins need a count, so that is one GPU.
func gpuCount(requests []container.DeviceRequest) int {
	count := 0
	for _, dr := range requests {
		if !slices.ContainsFunc(dr.Capabilities, func(caps []string) bool { return slices.Contains(caps, "gpu") }) {
			continue
		}
		if dr.Count < 0 {
			count = max(count, 1)
		} else {
			count = max(count, dr.Count)
		}
	}
	return count
}

// nativeSettings translates the settings Kubernetes has a pod or container
// field for. Only the fields it sets in the returned RunOptions are used.
func nativeSettings(cfg container.Config, host container.HostConfig) (RunOptions, []string) {
	var opts RunOptions
	var warnings []string

	opts.Hostname = cfg.Hostname
	if len(host.DNS) > 0 {
		servers := make([]string, len(host.DNS))
		for i, s := range host.DNS {
			servers[i] = s.String()
		}
		warnings = append(warnings, fmt.Sprintf("DNS servers %s ignored: the cluster DNS resolves names, which service discovery needs", strings.Join(servers, ", ")))
	}
	opts.DNSSearch = host.DNSSearch
	for _, o := range host.DNSOptions {
		name, value, hasValue := strings.Cut(o, ":")
		option := corev1.PodDNSConfigOption{Name: name}
		if hasValue {
			option.Value = &value
		}
		opts.DNSOptions = append(opts.DNSOptions, option)
	}
	for _, name := range slices.Sorted(maps.Keys(host.Sysctls)) {
		opts.Sysctls = append(opts.Sysctls, corev1.Sysctl{Name: name, Value: host.Sysctls[name]})
	}

	if host.ShmSize > 0 {
		opts.Tmpfs = append(opts.Tmpfs, TmpfsMount{Target: "/dev/shm", SizeLimit: resource.NewQuantity(host.ShmSize, resource.BinarySI)})
	}
	for _, target := range slices.Sorted(maps.Keys(host.Tmpfs)) {
		m := TmpfsMount{Target: target}
		var ignored []string
		for _, o := range strings.Split(host.Tmpfs[target], ",") {
			switch size, isSize := strings.CutPrefix(o, "size="); {
			case isSize:
				if q, err := resource.ParseQuantity(dockerSize(size)); err == nil {
					m.SizeLimit = &q
				} else {
					ignored = append(ignored, o)
				}
			case o != "" && o != "rw":
				ignored = append(ignored, o)
			}
		}
		if len(ignored) > 0 {
			warnings = append(warnings, fmt.Sprintf("tmpfs options %s at %q ignored: Kubernetes memory volumes take a size only", strings.Join(ignored, ","), target))
		}
		opts.Tmpfs = append(opts.Tmpfs, m)
	}
	for _, mnt := range host.Mounts {
		switch {
		case mnt.Type == mount.TypeTmpfs:
			m := TmpfsMount{Target: mnt.Target}
			if o := mnt.TmpfsOptions; o != nil && o.SizeBytes > 0 {
				m.SizeLimit = resource.NewQuantity(o.SizeBytes, resource.BinarySI)
			}
			opts.Tmpfs = append(opts.Tmpfs, m)
		case mnt.Type == mount.TypeVolume && mnt.VolumeOptions != nil && mnt.VolumeOptions.Subpath != "":
			if opts.VolumeSubpaths == nil {
				opts.VolumeSubpaths = map[string]string{}
			}
			opts.VolumeSubpaths[mnt.Target] = mnt.VolumeOptions.Subpath
		}
	}

	for _, o := range host.SecurityOpt {
		key, value, _ := strings.Cut(o, "=")
		if k, v, ok := strings.Cut(o, ":"); ok && k == "no-new-privileges" {
			key, value = k, v
		}
		switch {
		case key == "no-new-privileges" && (value == "" || value == "true"):
			opts.NoNewPrivileges = true
		case key == "seccomp" && value == "unconfined":
			opts.Seccomp = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
		case key == "seccomp":
			warnings = append(warnings, "seccomp profile ignored: Kubernetes loads seccomp profiles from the node")
		case key == "apparmor" && value == "unconfined":
			opts.AppArmor = &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}
		case key == "apparmor" && value == "docker-default":
			opts.AppArmor = &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeRuntimeDefault}
		case key == "apparmor" && value != "":
			opts.AppArmor = &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeLocalhost, LocalhostProfile: &value}
		case key == "label" && seLinuxLabel(&opts, value):
		default:
			warnings = append(warnings, fmt.Sprintf("security option %q is not supported by d2k and was ignored", o))
		}
	}

	for _, g := range host.GroupAdd {
		if id, err := strconv.ParseInt(g, 10, 64); err == nil {
			opts.SupplementalGroups = append(opts.SupplementalGroups, id)
		} else {
			warnings = append(warnings, fmt.Sprintf("group %q ignored: Kubernetes needs a numeric GID", g))
		}
	}
	opts.Annotations = host.Annotations
	return opts, warnings
}

// seLinuxLabel applies a label=<field>:<value> security option, and reports
// whether it is one Kubernetes can express.
func seLinuxLabel(opts *RunOptions, label string) bool {
	field, value, ok := strings.Cut(label, ":")
	if !ok {
		return false
	}
	if opts.SELinux == nil {
		opts.SELinux = &corev1.SELinuxOptions{}
	}
	switch field {
	case "user":
		opts.SELinux.User = value
	case "role":
		opts.SELinux.Role = value
	case "type":
		opts.SELinux.Type = value
	case "level":
		opts.SELinux.Level = value
	default:
		return false
	}
	return true
}

// dockerSize converts Docker's size suffixes (k, m, g, in powers of 1024)
// to Kubernetes quantities.
func dockerSize(s string) string {
	for suffix, unit := range map[string]string{"k": "Ki", "m": "Mi", "g": "Gi"} {
		if n, ok := strings.CutSuffix(strings.ToLower(s), suffix); ok {
			return n + unit
		}
	}
	return s
}
