// Package deploy checks invariants in deploy/kubernetes.yaml that a YAML
// diff review can easily miss.
package deploy

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

// d2kDeployment returns the d2k Deployment from kubernetes.yaml, which holds
// several documents separated by "---".
func d2kDeployment(t *testing.T) *appsv1.Deployment {
	t.Helper()
	data, err := os.ReadFile("kubernetes.yaml")
	if err != nil {
		t.Fatalf("read kubernetes.yaml: %v", err)
	}
	for _, doc := range strings.Split(string(data), "\n---") {
		var d appsv1.Deployment
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
			continue
		}
		if d.Kind == "Deployment" && d.Name == "d2k" {
			return &d
		}
	}
	t.Fatal("no Deployment named d2k in kubernetes.yaml")
	return nil
}

// TestPreStopDelaysSIGTERM guards against the fix for the resilience lane's
// lead finding: on "kubectl rollout restart" (or any pod delete), SIGTERM
// reaches d2k before every node's kube-proxy has removed the Terminating pod
// from the Service's endpoints, so a client that connects in that window
// gets its connection reset. A preStop sleep delays SIGTERM long enough for
// kube-proxy to converge while d2k keeps serving normally.
func TestPreStopDelaysSIGTERM(t *testing.T) {
	c := d2kDeployment(t).Spec.Template.Spec.Containers[0]
	if c.Lifecycle == nil || c.Lifecycle.PreStop == nil || c.Lifecycle.PreStop.Exec == nil {
		t.Fatal("d2k container has no preStop hook: SIGTERM reaches it as soon as termination starts, before kube-proxy stops routing to it")
	}
	cmd := c.Lifecycle.PreStop.Exec.Command
	// The d2k image is built FROM scratch (Dockerfile): it has no shell and no
	// coreutils, so a bare "sleep" exec fails with "executable file not found
	// in $PATH" (kubectl -n <ns> get events shows FailedPreStopHook) and the
	// hook returns immediately without delaying SIGTERM at all. The only
	// executable in the image is /d2k itself, so the hook must invoke that.
	if len(cmd) < 3 || cmd[0] != "/d2k" || cmd[1] != "sleep" {
		t.Fatalf("preStop command = %v, want [\"/d2k\", \"sleep\", N]: a bare \"sleep\" doesn't exist in the scratch image", cmd)
	}
	seconds, err := strconv.Atoi(cmd[len(cmd)-1])
	if err != nil || seconds < 5 {
		t.Fatalf("preStop sleep = %q, want at least 5 seconds", cmd[len(cmd)-1])
	}
}

// kinds returns the kind of every document in the named manifest.
func kinds(t *testing.T, file string) []string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var out []string
	for _, doc := range strings.Split(string(data), "\n---") {
		var meta struct{ Kind string }
		if err := yaml.Unmarshal([]byte(doc), &meta); err == nil && meta.Kind != "" {
			out = append(out, meta.Kind)
		}
	}
	return out
}

// The ValidatingAdmissionPolicy and its binding are cluster-scoped and
// serve every d2k install on the cluster, so they ship in their own file,
// applied once per cluster. In the per-install manifest, deleting one
// install (kubectl delete -f) deleted them for every other install.
func TestAdmissionPolicyShipsApartFromTheInstall(t *testing.T) {
	for _, k := range kinds(t, "kubernetes.yaml") {
		if k == "ValidatingAdmissionPolicy" || k == "ValidatingAdmissionPolicyBinding" {
			t.Errorf("kubernetes.yaml holds a %s: uninstalling one install deletes it for all", k)
		}
	}
	got := kinds(t, "admission-policy.yaml")
	if !slices.Equal(got, []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"}) {
		t.Errorf("admission-policy.yaml kinds = %v, want the policy and its binding", got)
	}
}

// The manifest installs Docker host mode, the mode d2k is built and
// supported for (compose, the Docker CLI, Portainer's standalone Docker
// environment). Swarm mode is opt-in: set D2K_SWARM_MODE to "true".
func TestManifestInstallsDockerHostMode(t *testing.T) {
	for _, e := range d2kDeployment(t).Spec.Template.Spec.Containers[0].Env {
		if e.Name == "D2K_SWARM_MODE" && e.Value != "false" {
			t.Errorf("D2K_SWARM_MODE = %q, want \"false\": Swarm mode is opt-in", e.Value)
		}
	}
}
