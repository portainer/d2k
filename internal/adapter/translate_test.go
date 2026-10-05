package adapter

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	corev1 "k8s.io/api/core/v1"
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
	// The body `docker create nginx:alpine` sends (Docker CLI 29.4).
	req := request(t, `{"AttachStdout":true,"AttachStderr":true,"Image":"nginx:alpine",
		"HostConfig":{"NetworkMode":"default","RestartPolicy":{"Name":"no"},"ConsoleSize":[0,0],"MemorySwappiness":-1},
		"NetworkingConfig":{"EndpointsConfig":{"default":{}}}}`)
	opts, warnings := RunOptionsFromRequest("web", req)
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
	if !slices.Equal(opts.PortBindings, []string{"127.0.0.1:5353:53", "8080:80"}) {
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
		"HostConfig":{"AutoRemove":true,"Dns":["1.1.1.1"],"RestartPolicy":{"Name":"on-failure","MaximumRetryCount":3},
			"LogConfig":{"Type":"syslog"},"ExtraHosts":["gw:host-gateway"],
			"Mounts":[{"Type":"bind","Source":"/srv","Target":"/srv"}],"PidsLimit":100}}`)
	_, warnings := RunOptionsFromRequest("web", req)
	text := strings.Join(warnings, "\n")
	for _, want := range []string{
		`user "nginx" ignored`, "Hostname is not supported", "Volumes is not supported",
		"HostConfig.AutoRemove is not supported", "HostConfig.Dns is not supported", "HostConfig.PidsLimit is not supported",
		"on-failure is not supported", `log driver "syslog"`, `extra host "gw:host-gateway" ignored`, `bind mount at "/srv" ignored`,
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
