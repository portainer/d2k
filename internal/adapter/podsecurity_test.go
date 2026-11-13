package adapter

import (
	"context"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/portainer/d2k/internal/types"
)

// portainerFormCreate is the HostConfig Portainer's create form sends with
// its default capabilities: Docker's defaults added, its other capabilities
// dropped.
const portainerFormCreate = `{"Image":"nginx","HostConfig":{
	"CapAdd":["SETPCAP","MKNOD","AUDIT_WRITE","CHOWN","NET_RAW","DAC_OVERRIDE","FOWNER","FSETID","KILL","SETGID","SETUID","NET_BIND_SERVICE","SYS_CHROOT","SETFCAP"],
	"CapDrop":["AUDIT_CONTROL","BLOCK_SUSPEND","DAC_READ_SEARCH","IPC_LOCK","IPC_OWNER","LEASE","LINUX_IMMUTABLE","MAC_ADMIN","MAC_OVERRIDE","NET_ADMIN","NET_BROADCAST","SYSLOG","SYS_ADMIN","SYS_BOOT","SYS_MODULE","SYS_NICE","SYS_PACCT","SYS_PTRACE","SYS_RAWIO","SYS_RESOURCE","SYS_TIME","SYS_TTY_CONFIG","WAKE_ALARM"]}}`

func createdPodSpec(t *testing.T, body string) corev1.PodSpec {
	t.Helper()
	a, client := newTestAdapter(t)
	opts, _ := RunOptionsFromRequest("app", request(t, body))
	mustCreate(t, a, opts)
	d, err := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "app", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d.Spec.Template.Spec
}

func addedCapabilities(spec corev1.PodSpec) []corev1.Capability {
	if sc := spec.Containers[0].SecurityContext; sc != nil && sc.Capabilities != nil {
		return sc.Capabilities.Add
	}
	return nil
}

// Adding a capability Docker grants by default changes nothing in Docker,
// but Pod Security's baseline level refuses a pod that adds NET_RAW: every
// container from Portainer's create form was refused on Talos.
func TestCreateContainerLeavesDockerDefaultCapabilitiesOut(t *testing.T) {
	spec := createdPodSpec(t, portainerFormCreate)

	if added := addedCapabilities(spec); len(added) != 0 {
		t.Errorf("capabilities added = %v, want none", added)
	}
	if dropped := spec.Containers[0].SecurityContext.Capabilities.Drop; !slices.Contains(dropped, "SYS_ADMIN") {
		t.Errorf("capabilities dropped = %v, want the form's drops kept", dropped)
	}
}

func TestCreateContainerKeepsTheCapabilitiesDockerAdds(t *testing.T) {
	for _, tt := range []struct {
		name       string
		hostConfig string
		want       []corev1.Capability
	}{
		{"a capability Docker doesn't grant", `{"CapAdd":["SYS_ADMIN","cap_net_raw"]}`, []corev1.Capability{"SYS_ADMIN"}},
		{"a default after dropping them all", `{"CapAdd":["CHOWN","NET_BIND_SERVICE"],"CapDrop":["ALL"]}`, []corev1.Capability{"CHOWN", "NET_BIND_SERVICE"}},
		{"a default also dropped", `{"CapAdd":["CAP_CHOWN"],"CapDrop":["chown"]}`, []corev1.Capability{"CHOWN"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := createdPodSpec(t, `{"Image":"nginx","HostConfig":`+tt.hostConfig+`}`)
			if added := addedCapabilities(spec); !slices.Equal(added, tt.want) {
				t.Errorf("capabilities added = %v, want %v", added, tt.want)
			}
		})
	}
}

// Docker applies its default seccomp profile to every container unless told
// seccomp=unconfined; Pod Security's restricted level requires one.
func TestCreateContainerUsesTheRuntimeDefaultSeccompProfile(t *testing.T) {
	spec := createdPodSpec(t, `{"Image":"nginx"}`)
	if sc := spec.SecurityContext; sc == nil || sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context = %+v, want the RuntimeDefault seccomp profile", spec.SecurityContext)
	}

	spec = createdPodSpec(t, `{"Image":"nginx","HostConfig":{"SecurityOpt":["seccomp=unconfined"]}}`)
	if sc := spec.Containers[0].SecurityContext; sc == nil || sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined {
		t.Errorf("container security context = %+v, want seccomp=unconfined kept", spec.Containers[0].SecurityContext)
	}
}

func TestInspectShowsNoSecurityOptionForTheDefaultSeccompProfile(t *testing.T) {
	a, _ := newTestAdapter(t)
	opts, _ := RunOptionsFromRequest("app", request(t, `{"Image":"nginx"}`))
	mustCreate(t, a, opts)

	inspect, err := a.InspectContainer(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if opts := inspect.HostConfig.SecurityOpt; len(opts) != 0 {
		t.Errorf("SecurityOpt = %v, want none", opts)
	}
}

// Pod Security's restricted level requires runAsNonRoot; a numeric user
// other than root asks for exactly that.
func TestCreateContainerWithANonRootUserRunsAsNonRoot(t *testing.T) {
	for _, tt := range []struct {
		user string
		want bool
	}{
		{"1000", true},
		{"1000:1000", true},
		{"0", false},
		{"0:1000", false},
	} {
		t.Run(tt.user, func(t *testing.T) {
			sc := createdPodSpec(t, `{"Image":"nginx","User":"`+tt.user+`"}`).Containers[0].SecurityContext
			got := sc != nil && sc.RunAsNonRoot != nil && *sc.RunAsNonRoot
			if got != tt.want {
				t.Errorf("runAsNonRoot = %v, want %v (security context %+v)", got, tt.want, sc)
			}
		})
	}
}

func TestSwarmServiceUsesTheRuntimeDefaultSeccompProfile(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	if _, err := a.SwarmCreateService(ctx, strings.NewReader(`{"Name":"web","TaskTemplate":{"ContainerSpec":{"Image":"nginx"}}}`)); err != nil {
		t.Fatal(err)
	}
	d, err := client.AppsV1().Deployments(testNamespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sc := d.Spec.Template.Spec.SecurityContext; sc == nil || sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context = %+v, want the RuntimeDefault seccomp profile", d.Spec.Template.Spec.SecurityContext)
	}
}

// Containers created from Portainer's form by d2k 1.3.0 add Docker's
// default capabilities, so Pod Security's baseline level refuses to start
// them. A stopped container's are removed on start-up; a running one is left
// alone, as changing its pod template would restart it.
func TestMigrateSecurityTranslationRemovesDefaultCapabilities(t *testing.T) {
	defaults := []corev1.Capability{"CHOWN", "NET_RAW", "SETUID"}
	withCaps := func(name string, replicas int32, add, drop []corev1.Capability) runtime.Object {
		d := legacyDeployment(name, replicas, nil)
		d.Annotations[types.AnnotationDockerName] = name
		d.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: add, Drop: drop}}
		return d
	}
	a, client := newTestAdapter(t,
		withCaps("stopped", 0, append(slices.Clone(defaults), "SYS_ADMIN"), []corev1.Capability{"NET_ADMIN"}),
		withCaps("running", 1, defaults, nil),
		withCaps("drops-all", 0, defaults, []corev1.Capability{"ALL"}),
	)
	ctx := context.Background()

	if err := a.MigrateSecurityTranslation(ctx); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string][]corev1.Capability{
		"stopped":   {"SYS_ADMIN"},
		"running":   defaults,
		"drops-all": defaults,
	} {
		d, err := client.AppsV1().Deployments(testNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := d.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities.Add; !slices.Equal(got, want) {
			t.Errorf("%s: capabilities added = %v, want %v", name, got, want)
		}
	}
}

// d2k 1.3.0 set no seccomp profile and no runAsNonRoot, which restricted
// requires: a stopped container created then with a non-root user gets
// both, as a new one does. A container's own seccomp=unconfined still wins.
func TestMigrateSecurityTranslationAddsSeccompAndRunAsNonRoot(t *testing.T) {
	legacy := legacyDeployment("app", 0, nil)
	legacy.Annotations[types.AnnotationDockerName] = "app"
	legacy.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
		RunAsUser:      new(int64(1000)),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
	}
	a, client := newTestAdapter(t, legacy)
	ctx := context.Background()

	if err := a.MigrateSecurityTranslation(ctx); err != nil {
		t.Fatal(err)
	}

	d, _ := client.AppsV1().Deployments(testNamespace).Get(ctx, "app", metav1.GetOptions{})
	spec := d.Spec.Template.Spec
	if sc := spec.SecurityContext; sc == nil || sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context = %+v, want the RuntimeDefault seccomp profile", spec.SecurityContext)
	}
	sc := spec.Containers[0].SecurityContext
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("runAsNonRoot = %v, want true for user 1000", sc.RunAsNonRoot)
	}
	if sc.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined {
		t.Errorf("container seccomp = %v, want its seccomp=unconfined kept", sc.SeccompProfile.Type)
	}
}

// A container that was running when d2k started keeps its 1.3.0 pod
// template; it is brought up to date when it is next started.
func TestStartContainerUpdatesAnEarlierReleasesSecuritySettings(t *testing.T) {
	legacy := legacyDeployment("app", 0, nil)
	legacy.Annotations[types.AnnotationDockerName] = "app"
	legacy.Annotations[types.AnnotationRestartPolicy] = "always"
	legacy.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_RAW"}}}
	a, client := newTestAdapter(t, legacy)
	ctx := context.Background()

	if err := a.StartContainer(ctx, "app"); err != nil {
		t.Fatal(err)
	}

	d, _ := client.AppsV1().Deployments(testNamespace).Get(ctx, "app", metav1.GetOptions{})
	if added := addedCapabilities(d.Spec.Template.Spec); len(added) != 0 {
		t.Errorf("capabilities added = %v, want none", added)
	}
	if sc := d.Spec.Template.Spec.SecurityContext; sc == nil || sc.SeccompProfile == nil {
		t.Errorf("pod security context = %+v, want the RuntimeDefault seccomp profile", sc)
	}
}

// The migration runs on every start: a container this release created has
// nothing to change.
func TestMigrateSecurityTranslationLeavesNewContainersAlone(t *testing.T) {
	a, client := newTestAdapter(t)
	opts, _ := RunOptionsFromRequest("app", request(t, portainerFormCreate))
	mustCreate(t, a, opts)
	before, _ := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "app", metav1.GetOptions{})

	if err := a.MigrateSecurityTranslation(context.Background()); err != nil {
		t.Fatal(err)
	}

	after, _ := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "app", metav1.GetOptions{})
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("resource version %q -> %q, want the Deployment untouched", before.ResourceVersion, after.ResourceVersion)
	}
}
