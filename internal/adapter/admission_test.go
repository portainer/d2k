package adapter

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/portainer/d2k/internal/errdefs"
)

// Refusals and warnings as the API server words them, captured on kind
// (Kubernetes 1.34) from server-side dry runs.
const (
	baselineRefusal = `violates PodSecurity "baseline:latest": forbidden AppArmor profile (container "app" must not set AppArmor profile type to "Unconfined"), non-default capabilities (container "app" must not include "NET_RAW", "SYS_ADMIN" in securityContext.capabilities.add), host namespaces (hostNetwork=true, hostPID=true, hostIPC=true), hostPath volumes (volume "h"), hostPort (container "app" uses hostPort 80), privileged (container "app" must not set securityContext.privileged=true), seLinuxOptions (container "app" set forbidden securityContext.seLinuxOptions: type "spc_t"), seccompProfile (container "app" must not set securityContext.seccompProfile.type to "Unconfined")`
	sysctlRefusal   = `violates PodSecurity "baseline:latest": procMount (container "app" must not set securityContext.procMount to "Unmasked"), forbidden sysctls (kernel.msgmax)`
	plainRestricted = `PodSecurity "restricted:latest": allowPrivilegeEscalation != false (container "app" must set securityContext.allowPrivilegeEscalation=false), unrestricted capabilities (container "app" must set securityContext.capabilities.drop=["ALL"]), runAsNonRoot != true (pod or container "app" must set securityContext.runAsNonRoot=true), seccompProfile (pod or container "app" must set securityContext.seccompProfile.type to "RuntimeDefault" or "Localhost")`
	rootRefusal     = `violates PodSecurity "restricted:latest": unrestricted capabilities (container "app" must not include "CHOWN" in securityContext.capabilities.add), restricted volume types (volume "n" uses restricted volume type "nfs"), runAsUser=0 (container "app" must not set runAsUser=0)`
)

func forbidden(reason string) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "app-x7k2q", errors.New(reason))
}

// refuseWith makes a's dry runs refused with err, and returns the pods
// they were given.
func refuseWith(a *KubernetesDockerAdapter, err error, warnings ...string) *[]*corev1.Pod {
	var pods []*corev1.Pod
	a.dryRunPod = func(_ context.Context, pod *corev1.Pod) ([]string, error) {
		pods = append(pods, pod)
		return warnings, err
	}
	return &pods
}

func TestPodSecurityRefusalNamesTheDockerSettings(t *testing.T) {
	for _, tt := range []struct {
		name   string
		reason string
		want   []string
	}{
		{"privileged", baselineRefusal, []string{"remove --privileged (compose: privileged)"}},
		{"added capabilities", baselineRefusal, []string{"remove NET_RAW, SYS_ADMIN from --cap-add (compose: cap_add)"}},
		{"unconfined seccomp", baselineRefusal, []string{"remove --security-opt seccomp=unconfined (compose: security_opt: [seccomp:unconfined])"}},
		{"unconfined AppArmor", baselineRefusal, []string{"remove --security-opt apparmor=unconfined (compose: security_opt: [apparmor:unconfined])"}},
		{"SELinux", baselineRefusal, []string{"remove --security-opt label=... (compose: security_opt: [label:...])"}},
		{"host path", baselineRefusal, []string{"use a named volume instead of a host path (compose: volumes)"}},
		{"host port", baselineRefusal, []string{"publish the port in ingress mode, not host mode (compose: ports: mode: host)"}},
		{"sysctls", sysctlRefusal, []string{"remove --sysctl kernel.msgmax (compose: sysctls)"}},
		{"restricted", "violates " + plainRestricted, []string{
			"add --security-opt no-new-privileges (compose: security_opt: [no-new-privileges:true])",
			"add --cap-drop ALL (compose: cap_drop: [ALL])",
			`run as a numeric non-root user, such as --user 1000:1000 (compose: user: "1000:1000")`,
			"re-create the container, so it gets Docker's default seccomp profile",
		}},
		{"root", rootRefusal, []string{
			"remove CHOWN from --cap-add (compose: cap_add); only NET_BIND_SERVICE may be added",
			`run as a numeric non-root user, such as --user 1000:1000 (compose: user: "1000:1000")`,
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := admissionResult(nil, forbidden(tt.reason))
			if !errdefs.IsForbidden(err) {
				t.Fatalf("error = %v, want Forbidden", err)
			}
			msg := err.Error()
			for _, want := range tt.want {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q\nlacks %q", msg, want)
				}
			}
			if level := tt.reason[strings.Index(tt.reason, `"`)+1 : strings.Index(tt.reason, ":")]; !strings.Contains(msg, `Pod Security level "`+level+`" refuses this container`) {
				t.Errorf("error %q doesn't name the level %q", msg, level)
			}
			if !strings.Contains(msg, "Kubernetes: "+tt.reason) {
				t.Errorf("error %q lacks Kubernetes' reason", msg)
			}
		})
	}
}

// The same advice for two checks is given once.
func TestPodSecurityRefusalGivesEachAdviceOnce(t *testing.T) {
	_, err := admissionResult(nil, forbidden(`violates PodSecurity "restricted:latest": runAsNonRoot != true (container "app" must not set securityContext.runAsNonRoot=false), runAsUser=0 (container "app" must not set runAsUser=0)`))
	if n := strings.Count(err.Error(), "--user 1000:1000"); n != 1 {
		t.Errorf("error %q gives the --user advice %d times, want once", err, n)
	}
}

func TestOtherAdmissionRefusalsPassUnchanged(t *testing.T) {
	for _, reason := range []string{
		`admission webhook "validate.kyverno.svc-fail" denied the request: resource Pod/d2k/app was blocked due to the following policies`,
		`ValidatingAdmissionPolicy 'no-latest' with binding 'no-latest' denied request: images must be pinned`,
	} {
		refusal := forbidden(reason)
		_, err := admissionResult(nil, refusal)
		if !errdefs.IsForbidden(err) || err.Error() != refusal.Error() {
			t.Errorf("error = %v, want Kubernetes' %q as Forbidden", err, refusal)
		}
	}
}

// Only policies refuse at create. A full quota, or a webhook that can't
// take dry runs, is left to the real create and start, as before.
func TestAdmissionIgnoresDryRunErrorsThatAreNotPolicies(t *testing.T) {
	for _, err := range []error{
		forbidden(`exceeded quota: d2k, requested: pods=1, used: pods=10, limited: pods=10`),
		apierrors.NewBadRequest(`admission webhook "mutate.example.com" does not support dry run`),
		apierrors.NewServiceUnavailable("etcdserver: request timed out"),
	} {
		if _, got := admissionResult(nil, err); got != nil {
			t.Errorf("admissionResult(%v) = %v, want no error", err, got)
		}
	}
}

func TestPodSecurityWarningNamesTheDockerSettings(t *testing.T) {
	warnings, err := admissionResult([]string{"would violate " + plainRestricted, "spec.terminationGracePeriodSeconds: must be >= 0"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %q, want 2", warnings)
	}
	if want := `the namespace's Pod Security level "restricted" would refuse this container, and only warns: add --security-opt no-new-privileges`; !strings.HasPrefix(warnings[0], want) {
		t.Errorf("warning %q\ndoesn't start %q", warnings[0], want)
	}
	if warnings[1] != "spec.terminationGracePeriodSeconds: must be >= 0" {
		t.Errorf("warning %q, want other warnings unchanged", warnings[1])
	}
}

func TestCreateContainerDryRunsThePodItWouldCreate(t *testing.T) {
	a, _ := newTestAdapter(t)
	pods := refuseWith(a, nil)
	opts, _ := RunOptionsFromRequest("app", request(t, `{"Image":"nginx","HostConfig":{"CapAdd":["SYS_ADMIN"]}}`))
	mustCreate(t, a, opts)

	if len(*pods) != 1 {
		t.Fatalf("dry runs = %d, want 1", len(*pods))
	}
	pod := (*pods)[0]
	if pod.Namespace != testNamespace || pod.GenerateName != "app-" {
		t.Errorf("pod %s/%s%s, want %s/app-<generated>", pod.Namespace, pod.GenerateName, pod.Name, testNamespace)
	}
	if added := addedCapabilities(pod.Spec); len(added) != 1 || added[0] != "SYS_ADMIN" {
		t.Errorf("dry-run pod adds %v, want the container's SYS_ADMIN", added)
	}
}

func TestCreateContainerRefusedByPodSecurityCreatesNothing(t *testing.T) {
	a, client := newTestAdapter(t)
	refuseWith(a, forbidden(baselineRefusal))
	opts, _ := RunOptionsFromRequest("app", request(t, `{"Image":"nginx","HostConfig":{"Privileged":true,"Binds":["data:/data"],"PortBindings":{"80/tcp":[{"HostPort":"8080"}]}}}`))

	_, _, err := a.CreateContainer(context.Background(), opts)

	if !errdefs.IsForbidden(err) || !strings.Contains(err.Error(), "remove --privileged") {
		t.Fatalf("error = %v, want Forbidden naming --privileged", err)
	}
	ctx := context.Background()
	deployments, _ := client.AppsV1().Deployments(testNamespace).List(ctx, metav1.ListOptions{})
	services, _ := client.CoreV1().Services(testNamespace).List(ctx, metav1.ListOptions{})
	claims, _ := client.CoreV1().PersistentVolumeClaims(testNamespace).List(ctx, metav1.ListOptions{})
	if n := len(deployments.Items) + len(services.Items) + len(claims.Items); n != 0 {
		t.Errorf("created %d Deployments, %d Services and %d claims, want none", len(deployments.Items), len(services.Items), len(claims.Items))
	}
}

func TestCreateContainerReturnsPodSecurityWarnings(t *testing.T) {
	a, _ := newTestAdapter(t)
	refuseWith(a, nil, "would violate "+plainRestricted)
	opts, _ := RunOptionsFromRequest("app", request(t, `{"Image":"nginx"}`))

	_, warnings, err := a.CreateContainer(context.Background(), opts)

	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), `Pod Security level "restricted" would refuse this container`) {
		t.Errorf("warnings = %q, want Pod Security's", warnings)
	}
}

// A container created before its namespace's level was raised, or by
// d2k 1.3.0, is refused at start with the same advice, and stays stopped.
func TestStartContainerRefusedByPodSecurity(t *testing.T) {
	for _, restart := range []string{"no", "always"} {
		t.Run(restart, func(t *testing.T) {
			a, client := newTestAdapter(t)
			opts, _ := RunOptionsFromRequest("app", request(t, `{"Image":"nginx","HostConfig":{"Privileged":true,"RestartPolicy":{"Name":"`+restart+`"}}}`))
			mustCreate(t, a, opts)
			refuseWith(a, forbidden(baselineRefusal))
			ctx := context.Background()

			err := a.StartContainer(ctx, "app")

			if !errdefs.IsForbidden(err) || !strings.Contains(err.Error(), "remove --privileged") {
				t.Fatalf("error = %v, want Forbidden naming --privileged", err)
			}
			d, _ := client.AppsV1().Deployments(testNamespace).Get(ctx, "app", metav1.GetOptions{})
			pods, _ := client.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{})
			if *d.Spec.Replicas != 0 || len(pods.Items) != 0 {
				t.Errorf("replicas = %d, pods = %d, want the container left stopped", *d.Spec.Replicas, len(pods.Items))
			}
		})
	}
}

func TestSwarmCreateServiceRefusedByPodSecurity(t *testing.T) {
	a, client := newTestAdapter(t)
	pods := refuseWith(a, forbidden("violates "+plainRestricted))
	ctx := context.Background()

	_, err := a.SwarmCreateService(ctx, strings.NewReader(`{"Name":"web","TaskTemplate":{"ContainerSpec":{"Image":"nginx"}}}`))

	if !errdefs.IsForbidden(err) || !strings.Contains(err.Error(), "--cap-drop ALL") {
		t.Fatalf("error = %v, want Forbidden naming --cap-drop ALL", err)
	}
	if len(*pods) != 1 || (*pods)[0].Spec.Containers[0].Image != "nginx" {
		t.Errorf("dry-run pods = %v, want the service's", *pods)
	}
	if deployments, _ := client.AppsV1().Deployments(testNamespace).List(ctx, metav1.ListOptions{}); len(deployments.Items) != 0 {
		t.Errorf("created %d Deployments, want none", len(deployments.Items))
	}
}
