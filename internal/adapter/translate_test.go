package adapter

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/docker/go-units"
	"github.com/moby/moby/api/types/container"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// request decodes a create request body as a client sends it.
func request(t *testing.T, body string) container.CreateRequest {
	t.Helper()
	var req container.CreateRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestPlainDockerRunHasNoWarnings(t *testing.T) {
	// The exact body `docker create nginx:alpine` sends (Docker CLI 29.4),
	// including its empty arrays, empty maps and false pointers.
	body, err := os.ReadFile("testdata/docker-create-plain.json")
	if err != nil {
		t.Fatal(err)
	}
	opts, warnings := RunOptionsFromRequest("web", request(t, string(body)))
	if len(warnings) != 0 {
		t.Fatalf("warnings = %q, want none", warnings)
	}
	if opts.Image != "nginx:alpine" || !slices.Equal(opts.Networks, []string{"default"}) {
		t.Fatalf("opts = %+v", opts)
	}
}

func TestTranslatedSettings(t *testing.T) {
	req := request(t, `{"Image":"app","User":"1000:2000","WorkingDir":"/srv","Tty":true,"OpenStdin":true,"StopTimeout":5,
		"ExposedPorts":{"80/tcp":{}},
		"HostConfig":{"Memory":268435456,"MemoryReservation":134217728,"NanoCpus":500000000,"CpuShares":512,
			"PortBindings":{"80/tcp":[{"HostPort":"8080"}],"53/udp":[{"HostIp":"127.0.0.1","HostPort":"5353"}]},
			"ExtraHosts":["db:10.0.0.5","cache:10.0.0.5","v6:[::1]"],"CapAdd":["NET_ADMIN"],"CapDrop":["ALL"],"ReadonlyRootfs":true,
			"Mounts":[{"Type":"volume","Source":"data","Target":"/data","ReadOnly":true}],
			"DeviceRequests":[{"Count":-1,"Capabilities":[["gpu"]]}]}}`)
	opts, warnings := RunOptionsFromRequest("web", req)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %q, want none", warnings)
	}
	if *opts.RunAsUser != 1000 || *opts.RunAsGroup != 2000 || opts.WorkingDir != "/srv" || !opts.Tty || !opts.Stdin || *opts.StopTimeout != 5 {
		t.Errorf("process settings = %+v", opts)
	}
	if got := opts.Resources.Limits.Memory().Value(); got != 256<<20 {
		t.Errorf("memory limit = %d", got)
	}
	if got := opts.Resources.Requests.Memory().Value(); got != 128<<20 {
		t.Errorf("memory request = %d", got)
	}
	if got := opts.Resources.Limits.Cpu().MilliValue(); got != 500 {
		t.Errorf("CPU limit = %dm, want 500m", got)
	}
	if got := opts.Resources.Requests.Cpu().MilliValue(); got != 500 {
		t.Errorf("CPU request = %dm, want 500m (512 shares)", got)
	}
	slices.Sort(opts.PortBindings)
	if !slices.Equal(opts.PortBindings, []string{"127.0.0.1:5353:53/udp", "8080:80/tcp"}) {
		t.Errorf("port bindings = %q", opts.PortBindings)
	}
	if _, ok := opts.ExposedPorts["80/tcp"]; !ok {
		t.Errorf("exposed ports = %v", opts.ExposedPorts)
	}
	wantHosts := []corev1.HostAlias{{IP: "10.0.0.5", Hostnames: []string{"db", "cache"}}, {IP: "::1", Hostnames: []string{"v6"}}}
	if len(opts.HostAliases) != 2 || !slices.Equal(opts.HostAliases[0].Hostnames, wantHosts[0].Hostnames) || opts.HostAliases[1].IP != "::1" {
		t.Errorf("host aliases = %+v, want %+v", opts.HostAliases, wantHosts)
	}
	if !slices.Equal(opts.CapAdd, []string{"NET_ADMIN"}) || !opts.ReadOnly {
		t.Errorf("security settings = %+v", opts)
	}
	if !slices.Equal(opts.Binds, []string{"data:/data:ro"}) || opts.GPUCount != 1 {
		t.Errorf("binds = %q, GPUs = %d", opts.Binds, opts.GPUCount)
	}
}

func TestUntranslatedSettingsWarn(t *testing.T) {
	req := request(t, `{"Image":"app","User":"nginx","Hostname":"h1","Healthcheck":{"Test":["CMD","true"]},"Volumes":{"/cache":{}},
		"HostConfig":{"Dns":["1.1.1.1"],"RestartPolicy":{"Name":"on-failure","MaximumRetryCount":3},
			"LogConfig":{"Type":"syslog"},"ExtraHosts":["gw:host-gateway"],
			"Mounts":[{"Type":"bind","Source":"/srv","Target":"/srv"}],"PidsLimit":100}}`)
	_, warnings := RunOptionsFromRequest("web", req)
	text := strings.Join(warnings, "\n")
	for _, want := range []string{
		`user "nginx" ignored`, "Volumes is not supported",
		"DNS servers 1.1.1.1 ignored", "HostConfig.PidsLimit is not supported",
		"maximum retry count 3 ignored", `log driver "syslog"`, `extra host "gw:host-gateway" ignored`, `bind mount at "/srv" ignored`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("warnings missing %q:\n%s", want, text)
		}
	}
}

func TestHealthcheckBecomesReadinessProbe(t *testing.T) {
	tests := []struct {
		name, body  string
		wantCommand []string
		wantWarning string
	}{
		{name: "CMD", body: `{"Test":["CMD","redis-cli","ping"],"Interval":5000000000,"Timeout":2000000000,"Retries":5,"StartPeriod":10000000000}`,
			wantCommand: []string{"redis-cli", "ping"}},
		{name: "CMD-SHELL", body: `{"Test":["CMD-SHELL","curl -f localhost || exit 1"]}`,
			wantCommand: []string{"/bin/sh", "-c", "curl -f localhost || exit 1"}},
		{name: "NONE", body: `{"Test":["NONE"]}`},
		{name: "inherited from the image", body: `{"Interval":5000000000}`, wantWarning: "image"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, warnings := RunOptionsFromRequest("db", request(t, `{"Image":"redis","Healthcheck":`+tt.body+`}`))
			if tt.wantWarning != "" {
				if opts.Healthcheck != nil || len(warnings) != 1 || !strings.Contains(warnings[0], tt.wantWarning) {
					t.Fatalf("probe = %+v, warnings = %q", opts.Healthcheck, warnings)
				}
				return
			}
			if len(warnings) != 0 {
				t.Fatalf("warnings = %q", warnings)
			}
			if tt.wantCommand == nil {
				if opts.Healthcheck != nil {
					t.Fatalf("probe = %+v, want none", opts.Healthcheck)
				}
				return
			}
			if !slices.Equal(opts.Healthcheck.Exec.Command, tt.wantCommand) {
				t.Fatalf("probe command = %q, want %q", opts.Healthcheck.Exec.Command, tt.wantCommand)
			}
		})
	}

	opts, _ := RunOptionsFromRequest("db", request(t, `{"Image":"redis","Healthcheck":{"Test":["CMD","true"],"Interval":5000000000,"Timeout":2000000000,"Retries":5,"StartPeriod":10000000000}}`))
	p := opts.Healthcheck
	if p.PeriodSeconds != 5 || p.TimeoutSeconds != 2 || p.FailureThreshold != 5 || p.InitialDelaySeconds != 10 {
		t.Errorf("probe timing = %+v", p)
	}
	opts, _ = RunOptionsFromRequest("db", request(t, `{"Image":"redis","Healthcheck":{"Test":["CMD","true"]}}`))
	if p := opts.Healthcheck; p.PeriodSeconds != 30 || p.TimeoutSeconds != 30 || p.FailureThreshold != 3 {
		t.Errorf("default probe timing = %+v, want Docker's 30s/30s/3", p)
	}
}

// Settings compose sends that d2k used to drop without a word.
func TestSilentlyDroppedSettingsWarn(t *testing.T) {
	tests := []struct {
		name, body  string
		wantWarning string
	}{
		{"network_mode host", `{"Image":"app","HostConfig":{"NetworkMode":"host"}}`, `network mode "host"`},
		{"network_mode none", `{"Image":"app","HostConfig":{"NetworkMode":"none"}}`, `network mode "none"`},
		{"network_mode service", `{"Image":"app","HostConfig":{"NetworkMode":"container:0123abcd"}}`, `network mode "container:0123abcd"`},
		{"ipv4_address", `{"Image":"app","NetworkingConfig":{"EndpointsConfig":{"p_front":{"IPAMConfig":{"IPv4Address":"172.30.0.10"}}}}}`, "IPAMConfig"},
		{"network mac_address", `{"Image":"app","NetworkingConfig":{"EndpointsConfig":{"p_front":{"MacAddress":"02:42:ac:11:00:02"}}}}`, "MacAddress"},
		{"network driver_opts", `{"Image":"app","NetworkingConfig":{"EndpointsConfig":{"p_front":{"DriverOpts":{"a":"b"}}}}}`, "DriverOpts"},
		{"network priority", `{"Image":"app","NetworkingConfig":{"EndpointsConfig":{"p_front":{"GwPriority":5}}}}`, "GwPriority"},
		{"mem_swappiness", `{"Image":"app","HostConfig":{"MemorySwappiness":10}}`, "MemorySwappiness"},
		{"logging options", `{"Image":"app","HostConfig":{"LogConfig":{"Type":"json-file","Config":{"max-size":"10m"}}}}`, "log options"},
		{"volume driver", `{"Image":"app","HostConfig":{"Mounts":[{"Type":"volume","Source":"data","Target":"/data","VolumeOptions":{"DriverConfig":{"Name":"rexray"}}}]}}`, `volume driver "rexray"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, warnings := RunOptionsFromRequest("web", request(t, tt.body))
			if !strings.Contains(strings.Join(warnings, "\n"), tt.wantWarning) {
				t.Fatalf("warnings = %q, want one about %s", warnings, tt.wantWarning)
			}
			for _, n := range opts.Networks {
				if n == "host" || n == "none" {
					t.Fatalf("networks = %q: a network mode is not a network", opts.Networks)
				}
			}
		})
	}

	// The defaults compose sends for a plain service are not settings.
	_, warnings := RunOptionsFromRequest("web", request(t, `{"Image":"app","HostConfig":{"NetworkMode":"p_default","LogConfig":{"Type":"json-file","Config":{}}},
		"NetworkingConfig":{"EndpointsConfig":{"p_default":{"Aliases":["p-web-1","web"],"IPAMConfig":{},"DriverOpts":{}}}}}`))
	if len(warnings) != 0 {
		t.Fatalf("warnings for a plain compose service = %q", warnings)
	}
}

// Settings Kubernetes has a field for.
func TestNativeTranslations(t *testing.T) {
	opts, warnings := RunOptionsFromRequest("web", request(t, `{"Image":"app","Hostname":"box",
		"HostConfig":{"Dns":["1.1.1.1"],"DnsSearch":["example.com"],"DnsOptions":["use-vc","ndots:2"],
			"Sysctls":{"net.core.somaxconn":"1024"},"ShmSize":134217728,"Tmpfs":{"/run":"rw,size=64m,noexec"},
			"SecurityOpt":["no-new-privileges:true","seccomp=unconfined","apparmor=unconfined","label=type:spc_t","label=level:s0:c1"],
			"GroupAdd":["1001","audio"],"Annotations":{"com.example.a":"b"},
			"Mounts":[{"Type":"tmpfs","Target":"/cache","TmpfsOptions":{"SizeBytes":1048576}},
				{"Type":"volume","Source":"data","Target":"/data","VolumeOptions":{"Subpath":"sub"}}]}}`))

	if opts.Hostname != "box" {
		t.Errorf("hostname = %q", opts.Hostname)
	}
	if !slices.Equal(opts.DNSSearch, []string{"example.com"}) || len(opts.DNSOptions) != 2 || opts.DNSOptions[1].Name != "ndots" || *opts.DNSOptions[1].Value != "2" {
		t.Errorf("DNS search = %q, options = %+v", opts.DNSSearch, opts.DNSOptions)
	}
	if len(opts.Sysctls) != 1 || opts.Sysctls[0] != (corev1.Sysctl{Name: "net.core.somaxconn", Value: "1024"}) {
		t.Errorf("sysctls = %+v", opts.Sysctls)
	}
	tmpfs := map[string]string{}
	for _, m := range opts.Tmpfs {
		tmpfs[m.Target] = ""
		if m.SizeLimit != nil {
			tmpfs[m.Target] = m.SizeLimit.String()
		}
	}
	if !maps.Equal(tmpfs, map[string]string{"/dev/shm": "128Mi", "/run": "64Mi", "/cache": "1Mi"}) {
		t.Errorf("memory mounts = %v", tmpfs)
	}
	if !opts.NoNewPrivileges || opts.Seccomp == nil || opts.Seccomp.Type != corev1.SeccompProfileTypeUnconfined ||
		opts.AppArmor == nil || opts.AppArmor.Type != corev1.AppArmorProfileTypeUnconfined ||
		opts.SELinux == nil || opts.SELinux.Type != "spc_t" || opts.SELinux.Level != "s0:c1" {
		t.Errorf("security = %v %+v %+v %+v", opts.NoNewPrivileges, opts.Seccomp, opts.AppArmor, opts.SELinux)
	}
	if !slices.Equal(opts.SupplementalGroups, []int64{1001}) || opts.Annotations["com.example.a"] != "b" {
		t.Errorf("groups = %v, annotations = %v", opts.SupplementalGroups, opts.Annotations)
	}
	if opts.VolumeSubpaths["/data"] != "sub" {
		t.Errorf("subpaths = %v", opts.VolumeSubpaths)
	}

	text := strings.Join(warnings, "\n")
	for _, want := range []string{`DNS servers 1.1.1.1 ignored`, `group "audio" ignored`, `tmpfs options noexec at "/run" ignored`} {
		if !strings.Contains(text, want) {
			t.Errorf("warnings missing %q:\n%s", want, text)
		}
	}
	if len(warnings) != 3 {
		t.Errorf("warnings = %q, want exactly 3", warnings)
	}

	_, warnings = RunOptionsFromRequest("web", request(t, `{"Image":"app","HostConfig":{"SecurityOpt":["seccomp={\"defaultAction\":\"SCMP_ACT_ERRNO\"}","apparmor=docker-default","label=disable"]}}`))
	if text := strings.Join(warnings, "\n"); !strings.Contains(text, "seccomp profile") || !strings.Contains(text, `"label=disable"`) || strings.Contains(text, "apparmor") {
		t.Errorf("warnings = %q, want the seccomp profile and label=disable to warn", warnings)
	}
}

// compose sends an anonymous volume (volumes: ["/data"]) as a volume mount
// with no source. It must not fail the create: it is ignored with a
// warning, as docker run's anonymous volumes (Config.Volumes) are.
func TestAnonymousVolumeWarns(t *testing.T) {
	a, _ := newTestAdapter(t)
	req := request(t, `{"Image":"app","HostConfig":{"Mounts":[{"Type":"volume","Target":"/data"},{"Type":"volume","Source":"named","Target":"/named"}]}}`)
	opts, warnings := RunOptionsFromRequest("app", req)
	if !slices.Equal(opts.Binds, []string{"named:/named"}) {
		t.Errorf("binds = %q, want the named volume only", opts.Binds)
	}
	if text := strings.Join(warnings, "\n"); !strings.Contains(text, `anonymous volume at "/data" ignored`) {
		t.Errorf("warnings = %q, want the anonymous volume named", warnings)
	}
	if _, _, err := a.CreateContainer(context.Background(), opts); err != nil {
		t.Fatalf("create with an anonymous volume: %v", err)
	}
}

// dockerSize must accept every size string github.com/docker/go-units
// RAMInBytes accepts (the "b"/"kb"/"kib" suffixes docker's --tmpfs and
// compose's tmpfs size= use), not just k/m/g.
func TestDockerSizeMatchesRAMInBytes(t *testing.T) {
	for _, size := range []string{"100b", "100B", "1kb", "1KB", "1kib", "1KiB", "64m", "2g", "0"} {
		want, err := units.RAMInBytes(size)
		if err != nil {
			t.Fatalf("units.RAMInBytes(%q): %v", size, err)
		}
		q, err := resource.ParseQuantity(dockerSize(size))
		if err != nil {
			t.Errorf("dockerSize(%q) = %q, not a valid quantity: %v", size, dockerSize(size), err)
			continue
		}
		if got := q.Value(); got != want {
			t.Errorf("dockerSize(%q) -> %d bytes, want %d (as RAMInBytes)", size, got, want)
		}
	}
}
