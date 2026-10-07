package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes/fake"
	appsv1client "k8s.io/client-go/kubernetes/typed/apps/v1"
	k8stesting "k8s.io/client-go/testing"

	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/internal/types"
)

func TestContainerID(t *testing.T) {
	id := containerID("2665dc61-cb7a-4c1e-9f00-000000000001")
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Fatalf("containerID() = %q, want 64 lowercase hex characters", id)
	}
	if id != containerID("2665dc61-cb7a-4c1e-9f00-000000000001") || id == containerID("2665dc61-cb7a-4c1e-9f00-000000000002") {
		t.Fatal("containerID() is not a stable, unique function of the UID")
	}
}

func TestResolveDeploymentName(t *testing.T) {
	const webUID, myWebUID = "2665dc61-cb7a-4c1e-9f00-000000000001", "7f3a9b20-1111-4c1e-9f00-000000000002"
	a, _ := newTestAdapter(t,
		deployment("web", webUID, managed()),
		deployment("my-web", myWebUID, managed()),
		deployment("my-api", "5b0c1d2e-3333-4c1e-9f00-000000000004", managed()),
		// d2k's own Deployment carries no managed-by label.
		deployment("d2k", "aaaaaaaa-2222-4c1e-9f00-000000000003", map[string]string{"app": "d2k"}),
	)
	webID, myWebID := containerID(webUID), containerID(myWebUID)

	tests := []struct {
		nameOrID string
		want     string
		wantErr  bool
	}{
		{nameOrID: "web", want: "web"},
		{nameOrID: "/web", want: "web"},
		{nameOrID: "my-web", want: "my-web"},
		{nameOrID: webID, want: "web"},
		{nameOrID: webID[:12], want: "web"},
		{nameOrID: myWebID[:5], want: "my-web"},
		// The Kubernetes UID is not a Docker ID.
		{nameOrID: webUID, wantErr: true},
		// Docker never resolves a name by suffix: "api" must not match "my-api".
		{nameOrID: "api", wantErr: true},
		{nameOrID: "", wantErr: true},
		// Unmanaged Deployments, including d2k itself, are invisible.
		{nameOrID: "d2k", wantErr: true},
		{nameOrID: containerID("aaaaaaaa-2222-4c1e-9f00-000000000003"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.nameOrID, func(t *testing.T) {
			got, err := a.resolveDeploymentName(context.Background(), tt.nameOrID)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveDeploymentName(%q) = %q, want error", tt.nameOrID, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveDeploymentName(%q) error: %v", tt.nameOrID, err)
			}
			if got != tt.want {
				t.Fatalf("resolveDeploymentName(%q) = %q, want %q", tt.nameOrID, got, tt.want)
			}
		})
	}
}

func TestResolveAmbiguousIDPrefix(t *testing.T) {
	a, _ := newTestAdapter(t)
	// Find two UIDs whose container IDs share a first character.
	var uids []string
	for i := 0; len(uids) < 2; i++ {
		uid := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		if len(uids) == 0 || containerID(k8stypes.UID(uid))[0] == containerID(k8stypes.UID(uids[0]))[0] {
			uids = append(uids, uid)
		}
	}
	for i, uid := range uids {
		if _, err := a.client.AppsV1().Deployments(testNamespace).Create(context.Background(), deployment(fmt.Sprintf("c%d", i), uid, managed()), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := a.resolveDeploymentName(context.Background(), containerID(k8stypes.UID(uids[0]))[:1])
	if !errdefs.IsInvalidParameter(err) {
		t.Fatalf("ambiguous prefix error = %v, want invalid parameter", err)
	}
}

func mustCreate(t *testing.T, a *KubernetesDockerAdapter, opts RunOptions) string {
	t.Helper()
	id, _, err := a.CreateContainer(context.Background(), opts)
	if err != nil {
		t.Fatalf("CreateContainer(%q) error: %v", opts.Name, err)
	}
	return id
}

func TestCreateContainerLabelsRoundTrip(t *testing.T) {
	a, _ := newTestAdapter(t)
	labels := map[string]string{
		// Longer than the 63-character Kubernetes label value limit.
		"com.docker.compose.config-hash": "ad9ce7678a0b66fda59648ea2d2c695da91708e6df59b63163cbf00b8213a667",
		// Not a valid Kubernetes label value.
		"com.docker.compose.project.working_dir": "/srv/app",
		"com.docker.compose.project":             "demo",
	}
	mustCreate(t, a, RunOptions{Name: "demo-web-1", Image: "nginx", Labels: labels})

	ctrs, err := a.ListContainers(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(ctrs[0].Labels, labels) {
		t.Fatalf("list Labels = %v, want %v", ctrs[0].Labels, labels)
	}
	inspect, err := a.InspectContainer(context.Background(), "demo-web-1")
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(inspect.Config.Labels, labels) {
		t.Fatalf("inspect Config.Labels = %v, want %v", inspect.Config.Labels, labels)
	}
}

func TestCreateContainerEntrypointAndCmd(t *testing.T) {
	a, client := newTestAdapter(t)
	mustCreate(t, a, RunOptions{Name: "cache", Image: "redis", Cmd: []string{"redis-server", "--save", ""}})
	mustCreate(t, a, RunOptions{Name: "tool", Image: "busybox", Entrypoint: []string{"sh", "-c"}, Cmd: []string{"echo hi"}})

	for name, want := range map[string][2][]string{
		// Cmd alone must not override the image ENTRYPOINT.
		"cache": {nil, {"redis-server", "--save", ""}},
		"tool":  {{"sh", "-c"}, {"echo hi"}},
	} {
		d, _ := client.AppsV1().Deployments(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
		c := d.Spec.Template.Spec.Containers[0]
		if !slices.Equal(c.Command, want[0]) || !slices.Equal(c.Args, want[1]) {
			t.Errorf("%s: command=%q args=%q, want command=%q args=%q", name, c.Command, c.Args, want[0], want[1])
		}
	}
}

func TestRenameContainerKeepsIdentity(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()
	id := mustCreate(t, a, RunOptions{Name: "demo-web-1", Image: "nginx"})

	// Compose recreate: create under a temporary name, remove the old
	// container, then rename the new one to the final name.
	tmpID := mustCreate(t, a, RunOptions{Name: id[:12] + "_demo-web-1", Image: "nginx"})
	if err := a.RemoveContainer(ctx, "demo-web-1"); err != nil {
		t.Fatal(err)
	}
	if err := a.RenameContainer(ctx, tmpID, "demo-web-1"); err != nil {
		t.Fatalf("RenameContainer() error: %v", err)
	}

	inspect, err := a.InspectContainer(ctx, "demo-web-1")
	if err != nil {
		t.Fatalf("InspectContainer(new name) error: %v", err)
	}
	if inspect.ID != tmpID || inspect.Name != "/demo-web-1" {
		t.Fatalf("after rename: ID=%q Name=%q, want ID=%q Name=/demo-web-1", inspect.ID, inspect.Name, tmpID)
	}
	if _, err := a.InspectContainer(ctx, id[:12]+"_demo-web-1"); err == nil {
		t.Fatal("old temporary name still resolves after rename")
	}

	// The freed name can be reused even though a Deployment is named after it.
	if err := a.RenameContainer(ctx, "demo-web-1", "web-old"); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, a, RunOptions{Name: "demo-web-1", Image: "nginx"})

	// Docker names stay unique.
	if _, _, err := a.CreateContainer(ctx, RunOptions{Name: "web-old", Image: "nginx"}); err == nil {
		t.Fatal("CreateContainer() reused a name that is in use")
	}
}

func TestCreateContainerWithPublishedPort(t *testing.T) {
	a, client := newTestAdapter(t)
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", PortBindings: []string{"18080:80"}})

	svcs, _ := client.CoreV1().Services(testNamespace).List(context.Background(), metav1.ListOptions{})
	ports := map[string][]int32{}
	for _, s := range svcs.Items {
		for _, p := range s.Spec.Ports {
			ports[s.Name] = append(ports[s.Name], p.Port)
		}
	}
	if !slices.Equal(ports["web"], []int32{80}) || !slices.Equal(ports["web-lb"], []int32{18080}) {
		t.Fatalf("Service ports = %v, want web:[80] web-lb:[18080]", ports)
	}
}

// Docker always fills a published port's HostIp (0.0.0.0 when none was
// given). An empty HostIp makes `docker port` print "invalid IP": the
// Docker CLI parses it as a netip.Addr and renders the zero value that way.
func TestInspectPortsHaveHostIP(t *testing.T) {
	a, _ := newTestAdapter(t)
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", PortBindings: []string{"18080:80"}})

	inspect, err := a.InspectContainer(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	bindings := inspect.NetworkSettings.Ports[network.MustParsePort("80/tcp")]
	if len(bindings) != 1 || bindings[0].HostIP.String() != "0.0.0.0" {
		t.Errorf("inspect NetworkSettings.Ports = %+v, want HostIp 0.0.0.0", bindings)
	}
}

// docker inspect's RestartCount, and the restart count Portainer reads from
// it, must reflect a crash-looping container's restarts rather than stay 0
// (D2K-080).
func TestInspectReportsRestartCount(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", RestartPolicy: "always"})
	if err := a.StartContainer(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: testNamespace,
		Labels: map[string]string{"app": "web", types.LabelManagedBy: types.LabelManagedByValue}}}
	if _, err := client.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod.Status = corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		RestartCount: 4,
		State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	}}}
	if _, err := client.CoreV1().Pods(testNamespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	inspect, err := a.InspectContainer(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if inspect.RestartCount != 4 {
		t.Errorf("RestartCount = %d, want 4 for a crash-looping restart:always container", inspect.RestartCount)
	}
}

func TestCreateContainerNamedVolumes(t *testing.T) {
	a, client := newTestAdapter(t)
	_, warnings, err := a.CreateContainer(context.Background(), RunOptions{
		Name: "web", Image: "nginx",
		Binds: []string{"demo_data:/data:rw", "logs:/var/log:ro", "/srv/html:/usr/share/nginx/html"},
	})
	if err != nil {
		t.Fatal(err)
	}

	d, _ := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "web", metav1.GetOptions{})
	pod := d.Spec.Template.Spec
	claims := map[string]string{}
	for _, v := range pod.Volumes {
		claims[v.Name] = v.PersistentVolumeClaim.ClaimName
	}
	mounts := map[string]string{}
	for _, m := range pod.Containers[0].VolumeMounts {
		mounts[m.MountPath] = claims[m.Name]
		if m.MountPath == "/var/log" && !m.ReadOnly {
			t.Error("ro bind mounted read-write")
		}
	}
	if !maps.Equal(mounts, map[string]string{"/data": "demo-data", "/var/log": "logs"}) {
		t.Fatalf("mounts = %v, want /data→demo-data, /var/log→logs", mounts)
	}
	// Docker creates missing named volumes on run.
	if _, err := client.CoreV1().PersistentVolumeClaims(testNamespace).Get(context.Background(), "logs", metav1.GetOptions{}); err != nil {
		t.Fatalf("PVC for named volume not created: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "/srv/html") {
		t.Fatalf("warnings = %q, want one about the host path bind", warnings)
	}
	// Mounts name the Docker volume, which compose compares with its own.
	names := map[string]string{}
	for _, m := range containerMounts(*d) {
		names[m.Destination] = m.Name
	}
	if !maps.Equal(names, map[string]string{"/data": "demo_data", "/var/log": "logs"}) {
		t.Fatalf("mount names = %v, want /data→demo_data, /var/log→logs", names)
	}
}

func TestComposeServiceNamesAreScopedToTheirProject(t *testing.T) {
	a, client := newTestAdapter(t)
	a.clusterDNS, a.clusterDomain = "10.96.0.10", "cluster.local"
	ctx := context.Background()
	compose := func(project, name string) RunOptions {
		return RunOptions{
			Name: name, Image: "redis",
			Labels:  map[string]string{"com.docker.compose.project": project, "com.docker.compose.service": "cache"},
			Aliases: []string{name, "cache"},
		}
	}
	// Two projects with the same service name must both deploy.
	mustCreate(t, a, compose("demo", "demo-cache-1"))
	mustCreate(t, a, compose("other", "other-cache-1"))

	for project, deploy := range map[string]string{"demo": "demo-cache-1", "other": "other-cache-1"} {
		d, _ := client.AppsV1().Deployments(testNamespace).Get(ctx, deploy, metav1.GetOptions{})
		pod := d.Spec.Template.Spec
		subdomain := "compose-" + project
		if pod.Hostname != "cache" || pod.Subdomain != subdomain {
			t.Errorf("%s: hostname=%q subdomain=%q, want cache, %s", deploy, pod.Hostname, pod.Subdomain, subdomain)
		}
		// The project's own names are searched before the namespace's.
		if pod.DNSConfig == nil || pod.DNSConfig.Searches[0] != subdomain+".d2k.svc.cluster.local" {
			t.Errorf("%s: DNS config = %+v, want the project domain searched first", deploy, pod.DNSConfig)
		}
		svc, err := client.CoreV1().Services(testNamespace).Get(ctx, subdomain, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("no DNS Service for project %s: %v", project, err)
		}
		for k, v := range svc.Spec.Selector {
			if d.Spec.Template.Labels[k] != v {
				t.Errorf("project Service selector %v does not select %s (labels %v)", svc.Spec.Selector, deploy, d.Spec.Template.Labels)
			}
		}
	}

	// Every container of the project owns its DNS Service, so Kubernetes
	// deletes the Service with the project's last container.
	mustCreate(t, a, compose("demo", "demo-cache-2"))
	svc, _ := client.CoreV1().Services(testNamespace).Get(ctx, "compose-demo", metav1.GetOptions{})
	var owners []string
	for _, ref := range svc.OwnerReferences {
		owners = append(owners, ref.Kind+"/"+ref.Name)
	}
	if !slices.Equal(owners, []string{"Deployment/demo-cache-1", "Deployment/demo-cache-2"}) {
		t.Fatalf("project DNS Service owners = %v, want both demo Deployments", owners)
	}
}

func TestContainerNetworkAttachments(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", Networks: []string{"demo_back"}})

	// Compose attaches additional networks after create, by name or by ID.
	if err := a.ConnectNetwork(ctx, networkIDForName("demo_front", testNamespace), "web", true); err != nil {
		t.Fatal(err)
	}
	ctrs, _ := a.ListContainers(ctx, true)
	want := map[string]string{
		"demo_back":  networkIDForName("demo_back", testNamespace),
		"demo_front": networkIDForName("demo_front", testNamespace),
	}
	if !maps.Equal(ctrs[0].Networks, want) {
		t.Fatalf("Networks = %v, want %v", ctrs[0].Networks, want)
	}

	if err := a.ConnectNetwork(ctx, "demo_back", "web", false); err != nil {
		t.Fatal(err)
	}
	inspect, _ := a.InspectContainer(ctx, "web")
	if _, ok := inspect.NetworkSettings.Networks["demo_back"]; ok || len(inspect.NetworkSettings.Networks) != 1 {
		t.Fatalf("after disconnect Networks = %v, want only demo_front", inspect.NetworkSettings.Networks)
	}
}

// Portainer's Duplicate/Edit flow reconnects a container's "extra networks"
// concurrently (Promise.all over one POST /networks/{id}/connect per
// network), exactly like compose attaching several networks after create.
// ConnectNetwork's read-modify-write (Get the Deployment, compute the new
// network list, write the whole annotation back) has no retry on conflict,
// so when a second connect's Get-to-write lands inside the first's, the
// second call's write is silently lost.
//
// A real race between two goroutines can't be forced through the fake
// clientset (its Invokes holds one lock for the whole reaction chain, so
// blocking inside a reactor deadlocks any other action), so this reproduces
// the interleaving deterministically: a reactor on the first call's write
// writes a second network straight to the tracker, simulating the other
// connect's Get-and-write completing in the gap. The reactor fires on
// either "patch" or "update" so the same test demonstrates the bug against
// a blind write and the retry-on-conflict fix against a conflict-checked
// one.
func TestConcurrentConnectNetworkLosesAnUpdate(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", Networks: []string{"demo_base"}})

	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	gr := schema.GroupResource{Group: "apps", Resource: "deployments"}
	injected := false
	raceReactor := func(action k8stesting.Action) (bool, runtime.Object, error) {
		if injected {
			return false, nil, nil
		}
		injected = true

		obj, err := client.Tracker().Get(gvr, testNamespace, "web")
		if err != nil {
			t.Fatal(err)
		}
		d := obj.(*appsv1.Deployment)
		networks := append(containerNetworks(*d), "demo_side")
		encoded, err := json.Marshal(networks)
		if err != nil {
			t.Fatal(err)
		}
		d.Annotations[types.AnnotationNetworks] = string(encoded)
		if err := client.Tracker().Update(gvr, d, testNamespace); err != nil {
			t.Fatal(err)
		}

		if action.GetVerb() == "update" {
			// A conflict-checked write sees the injected change and must retry.
			return true, nil, apierrors.NewConflict(gr, "web", errors.New("concurrent connect"))
		}
		return false, nil, nil
	}
	client.PrependReactor("patch", "deployments", raceReactor)
	client.PrependReactor("update", "deployments", raceReactor)

	if err := a.ConnectNetwork(ctx, "demo_front", "web", true); err != nil {
		t.Fatal(err)
	}

	ctrs, err := a.ListContainers(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"demo_base":  networkIDForName("demo_base", testNamespace),
		"demo_front": networkIDForName("demo_front", testNamespace),
		"demo_side":  networkIDForName("demo_side", testNamespace),
	}
	if !maps.Equal(ctrs[0].Networks, want) {
		t.Fatalf("Networks after two interleaved connects = %v, want %v (one connect's write overwrote the other's)", ctrs[0].Networks, want)
	}
}

func TestCreateDoesNotStart(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx"})

	d, _ := client.AppsV1().Deployments(testNamespace).Get(ctx, "web", metav1.GetOptions{})
	if *d.Spec.Replicas != 0 {
		t.Fatalf("replicas after create = %d, want 0", *d.Spec.Replicas)
	}
	inspect, _ := a.InspectContainer(ctx, "web")
	if inspect.State.Status != "created" {
		t.Fatalf("state after create = %s, want created", inspect.State.Status)
	}

	if err := a.StartContainer(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	d, _ = client.AppsV1().Deployments(testNamespace).Get(ctx, "web", metav1.GetOptions{})
	if *d.Spec.Replicas != 1 || d.Annotations[types.AnnotationStartedAt] == "" {
		t.Fatalf("after start: replicas = %d, started-at = %q", *d.Spec.Replicas, d.Annotations[types.AnnotationStartedAt])
	}

	if err := a.StopContainer(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	inspect, _ = a.InspectContainer(ctx, "web")
	if inspect.State.Status != "exited" || inspect.State.FinishedAt == "" {
		t.Fatalf("after stop: state = %s, finished at %q; want exited with a time", inspect.State.Status, inspect.State.FinishedAt)
	}
}

func TestCreateContainerAppliesTranslatedSettings(t *testing.T) {
	a, client := newTestAdapter(t)
	opts, warnings := RunOptionsFromRequest("app", request(t, `{"Image":"app","User":"1000","WorkingDir":"/srv","Tty":true,"StopTimeout":5,
		"HostConfig":{"Memory":268435456,"NanoCpus":500000000,"ExtraHosts":["db:10.0.0.5"],"CapDrop":["ALL"],"ReadonlyRootfs":true}}`))
	if len(warnings) != 0 {
		t.Fatal(warnings)
	}
	mustCreate(t, a, opts)

	d, _ := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "app", metav1.GetOptions{})
	pod := d.Spec.Template.Spec
	c := pod.Containers[0]
	if c.WorkingDir != "/srv" || !c.TTY || *pod.TerminationGracePeriodSeconds != 5 {
		t.Errorf("container = %+v, grace = %v", c, pod.TerminationGracePeriodSeconds)
	}
	if *c.SecurityContext.RunAsUser != 1000 || !*c.SecurityContext.ReadOnlyRootFilesystem || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("security context = %+v", c.SecurityContext)
	}
	if c.Resources.Limits.Memory().Value() != 256<<20 || c.Resources.Limits.Cpu().MilliValue() != 500 {
		t.Errorf("resources = %+v", c.Resources)
	}
	if len(pod.HostAliases) != 1 || pod.HostAliases[0].IP != "10.0.0.5" {
		t.Errorf("host aliases = %+v", pod.HostAliases)
	}
}

func TestListContainersUsesConstantAPICalls(t *testing.T) {
	a, client := newTestAdapter(t)
	for i := range 20 {
		mustCreate(t, a, RunOptions{Name: fmt.Sprintf("c%d", i), Image: "nginx", PortBindings: []string{fmt.Sprintf("%d:80", 18000+i)}})
	}
	client.ClearActions()
	if _, err := a.ListContainers(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if n := len(client.Actions()); n > 3 {
		t.Fatalf("ListContainers made %d API calls for 20 containers, want at most 3 (deployments, pods, services)", n)
	}
}

func TestCreateContainerWarnsWithoutGPUs(t *testing.T) {
	a, _ := newTestAdapter(t)
	_, warnings, err := a.CreateContainer(context.Background(), RunOptions{Name: "ml", Image: "app", GPUCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "GPU") {
		t.Fatalf("warnings = %q, want one about GPUs the cluster does not offer", warnings)
	}
}

// Compose discards network create warnings, so network options d2k cannot
// honour are reported on the containers that join the network.
func TestUnsupportedNetworkOptionsWarnOnJoin(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()
	if _, _, err := a.CreateNetwork(ctx, CreateNetworkOptions{Name: "p_back", Internal: true, IPAMConfigured: true, EnableIPv6: true, DisableIPv4: true}); err != nil {
		t.Fatal(err)
	}
	_, warnings, err := a.CreateContainer(ctx, RunOptions{Name: "db", Image: "redis", Networks: []string{"p_back"}})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(warnings, "\n")
	for _, want := range []string{"internal", "IPAM", "IPv6", "IPv4"} {
		if !strings.Contains(text, want) {
			t.Errorf("warnings = %q, want one about %s", warnings, want)
		}
	}

	for _, driver := range []string{"macvlan", "ipvlan"} {
		if _, _, err := a.CreateNetwork(ctx, CreateNetworkOptions{Name: "lan", Driver: driver}); !errdefs.IsInvalidParameter(err) {
			t.Errorf("create %s network = %v, want invalid parameter", driver, err)
		}
	}
}

// Compose creates a network when inspecting it fails, so unknown networks
// must not exist. Networks live in memory: those that containers joined stay
// known after d2k restarts.
func TestInspectNetwork(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	if _, err := a.InspectNetwork(ctx, "p_unknown"); !errdefs.IsNotFound(err) {
		t.Fatalf("inspect of an unknown network = %v, want not found", err)
	}
	if _, _, err := a.CreateContainer(ctx, RunOptions{Name: "web", Image: "nginx", Networks: []string{"p_front"}}); err != nil {
		t.Fatal(err)
	}
	restarted := NewWithClient(client, testNamespace)
	n, err := restarted.InspectNetwork(ctx, "p_front")
	if err != nil || n.Labels["com.docker.compose.project"] != "p" {
		t.Fatalf("inspect of a joined network after restart = %+v, %v", n, err)
	}
}

func TestCreateContainerNativeSettings(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	two := "2"
	size := resource.MustParse("64Mi")
	opts := RunOptions{
		Name: "web", Image: "nginx", Hostname: "box", Binds: []string{"data:/data"},
		DNSSearch: []string{"example.com"}, DNSOptions: []corev1.PodDNSConfigOption{{Name: "ndots", Value: &two}},
		Sysctls:         []corev1.Sysctl{{Name: "net.core.somaxconn", Value: "1024"}},
		Tmpfs:           []TmpfsMount{{Target: "/dev/shm", SizeLimit: &size}, {Target: "/run"}},
		NoNewPrivileges: true, Seccomp: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		SupplementalGroups: []int64{1001}, Annotations: map[string]string{"com.example.a": "b"},
		VolumeSubpaths: map[string]string{"/data": "sub"},
	}
	if _, _, err := a.CreateContainer(ctx, opts); err != nil {
		t.Fatal(err)
	}
	d, _ := client.AppsV1().Deployments(testNamespace).Get(ctx, "web", metav1.GetOptions{})
	pod := d.Spec.Template
	c := pod.Spec.Containers[0]

	if pod.Spec.Hostname != "box" || pod.Annotations["com.example.a"] != "b" {
		t.Errorf("hostname = %q, annotations = %v", pod.Spec.Hostname, pod.Annotations)
	}
	if dns := pod.Spec.DNSConfig; dns == nil || !slices.Equal(dns.Searches, []string{"example.com"}) || len(dns.Options) != 1 {
		t.Errorf("DNS config = %+v", pod.Spec.DNSConfig)
	}
	if sc := pod.Spec.SecurityContext; sc == nil || len(sc.Sysctls) != 1 || !slices.Equal(sc.SupplementalGroups, []int64{1001}) {
		t.Errorf("pod security context = %+v", pod.Spec.SecurityContext)
	}
	if sc := c.SecurityContext; sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.SeccompProfile == nil {
		t.Errorf("container security context = %+v", c.SecurityContext)
	}
	memory := map[string]string{}
	for _, v := range pod.Spec.Volumes {
		if v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory {
			for _, m := range c.VolumeMounts {
				if m.Name == v.Name {
					memory[m.MountPath] = ""
					if v.EmptyDir.SizeLimit != nil {
						memory[m.MountPath] = v.EmptyDir.SizeLimit.String()
					}
				}
			}
		}
	}
	if !maps.Equal(memory, map[string]string{"/dev/shm": "64Mi", "/run": ""}) {
		t.Errorf("memory mounts = %v", memory)
	}
	for _, m := range c.VolumeMounts {
		if m.MountPath == "/data" && m.SubPath != "sub" {
			t.Errorf("/data subPath = %q, want sub", m.SubPath)
		}
	}

	// A compose service keeps its service name as hostname, for its DNS, and
	// searches its project's domain first.
	a.clusterDNS, a.clusterDomain = "10.96.0.10", "cluster.local"
	opts = RunOptions{Name: "p-api-1", Image: "nginx", Hostname: "box", DNSSearch: []string{"example.com"},
		Labels: map[string]string{"com.docker.compose.project": "p", "com.docker.compose.service": "api"}}
	_, warnings, err := a.CreateContainer(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	d, _ = client.AppsV1().Deployments(testNamespace).Get(ctx, "p-api-1", metav1.GetOptions{})
	if d.Spec.Template.Spec.Hostname != "api" || !strings.Contains(strings.Join(warnings, "\n"), `hostname "box" ignored`) {
		t.Errorf("compose hostname = %q, warnings = %q", d.Spec.Template.Spec.Hostname, warnings)
	}
	if s := d.Spec.Template.Spec.DNSConfig.Searches; s[len(s)-1] != "example.com" || !strings.HasPrefix(s[0], "compose-p.") {
		t.Errorf("compose DNS searches = %q", s)
	}
}

func TestUDPPortsKeepTheirProtocol(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	// pihole's ports: DNS on 53 over TCP and UDP.
	req := request(t, `{"Image":"pihole/pihole","ExposedPorts":{"53/tcp":{},"53/udp":{},"80/tcp":{}},
		"HostConfig":{"PortBindings":{"53/tcp":[{"HostPort":"53"}],"53/udp":[{"HostPort":"53"}],"80/tcp":[{"HostPort":"8080"}]}}}`)
	opts, _ := RunOptionsFromRequest("pihole", req)
	mustCreate(t, a, opts)

	for _, name := range []string{serviceName("pihole"), publishedServiceName("pihole")} {
		svc, err := client.CoreV1().Services(testNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, p := range svc.Spec.Ports {
			key := fmt.Sprintf("%d/%s", p.Port, p.Protocol)
			if seen[key] {
				t.Errorf("Service %s: duplicate port %s, which Kubernetes rejects", name, key)
			}
			seen[key] = true
		}
		if !seen["53/UDP"] || !seen["53/TCP"] {
			t.Errorf("Service %s ports = %v, want 53/TCP and 53/UDP", name, seen)
		}
	}

	ctrs, err := a.ListContainers(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(ctrs[0].Ports, func(p container.PortSummary) bool {
		return p.PrivatePort == 53 && p.PublicPort == 53 && p.Type == "udp"
	}) {
		t.Errorf("list Ports = %+v, want 53->53/udp", ctrs[0].Ports)
	}
	inspect, err := a.InspectContainer(ctx, "pihole")
	if err != nil {
		t.Fatal(err)
	}
	if b := inspect.HostConfig.PortBindings[network.MustParsePort("53/udp")]; len(b) != 1 || b[0].HostPort != "53" {
		t.Errorf("inspect PortBindings = %v, want 53/udp -> 53", inspect.HostConfig.PortBindings)
	}
}

// Docker publishes a port given without a host port on a port it picks:
// compose's `ports: ["80"]` and `docker run -p 80`. d2k publishes it on the
// container port.
func TestPortWithoutHostPort(t *testing.T) {
	a, client := newTestAdapter(t)
	req := request(t, `{"Image":"nginx","ExposedPorts":{"80/tcp":{}},"HostConfig":{"PortBindings":{"80/tcp":[{"HostIp":"","HostPort":""}]}}}`)
	opts, _ := RunOptionsFromRequest("web", req)
	if _, _, err := a.CreateContainer(context.Background(), opts); err != nil {
		t.Fatalf("create with port 80 and no host port: %v", err)
	}
	svc, err := client.CoreV1().Services(testNamespace).Get(context.Background(), publishedServiceName("web"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntValue() != 80 {
		t.Fatalf("published ports = %+v, want 80 -> 80", svc.Spec.Ports)
	}
	ctrs, _ := a.ListContainers(context.Background(), true)
	if len(ctrs[0].Ports) != 1 || ctrs[0].Ports[0].PublicPort != 80 {
		t.Fatalf("list Ports = %+v, want 80 published on 80", ctrs[0].Ports)
	}
}

// A container acts only on its own Services: a Service of the same name
// that belongs to another container, or not to d2k, is neither taken over
// nor deleted.
func TestContainerLeavesOtherServicesAlone(t *testing.T) {
	foreign := func(name string) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "helm-" + name}}}
	}
	a, client := newTestAdapter(t, foreign("db"), foreign("cache-lb"))
	ctx := context.Background()
	exists := func(name string) bool {
		_, err := client.CoreV1().Services(testNamespace).Get(ctx, name, metav1.GetOptions{})
		return err == nil
	}

	// "cache" has no published ports, so "cache-lb" is not its Service.
	mustCreate(t, a, RunOptions{Name: "cache", Image: "redis"})
	if err := a.RemoveContainer(ctx, "cache"); err != nil {
		t.Fatal(err)
	}
	if !exists("cache-lb") {
		t.Error("docker rm cache deleted Service cache-lb, which is not its")
	}

	// "db" would resolve to the other workload's pods.
	if _, _, err := a.CreateContainer(ctx, RunOptions{Name: "db", Image: "postgres"}); !errdefs.IsConflict(err) {
		t.Errorf("create over another workload's Service = %v, want conflict", err)
	}
	if svc, err := client.CoreV1().Services(testNamespace).Get(ctx, "db", metav1.GetOptions{}); err != nil || svc.Spec.Selector["app"] != "helm-db" {
		t.Errorf("Service db after the refused create = %+v, %v; want it unchanged", svc, err)
	}

	// "web-lb" is the name of web's published Service.
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", PortBindings: []string{"8080:80/tcp"}})
	if _, _, err := a.CreateContainer(ctx, RunOptions{Name: "web-lb", Image: "nginx"}); !errdefs.IsConflict(err) {
		t.Errorf("create of web-lb over web's published Service = %v, want conflict", err)
		_ = a.RemoveContainer(ctx, "web-lb")
	}
	if !exists("web-lb") {
		t.Error("web's published Service web-lb was deleted")
	}
}

// Docker label keys need not be Kubernetes label keys: Traefik's are often
// longer than 63 characters. They are kept as the container's and volume's
// Docker labels, and only valid keys become Kubernetes labels, which the
// API server would otherwise reject.
func TestLabelKeysKubernetesRejects(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	labels := map[string]string{
		"traefik.http.middlewares.redirect-to-https.redirectscheme.scheme": "https",
		"traefik.enable": "true",
	}
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", Labels: labels})
	d, _ := client.AppsV1().Deployments(testNamespace).Get(ctx, "web", metav1.GetOptions{})
	if errs := metav1validation.ValidateLabels(d.Labels, field.NewPath("metadata", "labels")); len(errs) > 0 {
		t.Errorf("Deployment labels the API server rejects: %v", errs)
	}
	if d.Labels["traefik.enable"] != "true" {
		t.Errorf("Deployment labels = %v, want the valid Docker label kept", d.Labels)
	}
	if ctr, _ := a.InspectContainer(ctx, "web"); !maps.Equal(ctr.Config.Labels, labels) {
		t.Errorf("Docker labels = %v, want %v", ctr.Config.Labels, labels)
	}

	if _, err := a.CreateVolume(ctx, CreateVolumeOptions{Name: "data", Labels: labels}); err != nil {
		t.Fatal(err)
	}
	pvc, _ := client.CoreV1().PersistentVolumeClaims(testNamespace).Get(ctx, "data", metav1.GetOptions{})
	if errs := metav1validation.ValidateLabels(pvc.Labels, field.NewPath("metadata", "labels")); len(errs) > 0 {
		t.Errorf("PVC labels the API server rejects: %v", errs)
	}
}

// As in Docker, docker stop returns once the container has stopped: compose
// recreates a service by stopping the old container, then starting the new
// one on the same volumes.
func TestStopWaitsForThePod(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	mustCreate(t, a, RunOptions{Name: "db", Image: "postgres", RestartPolicy: "always"})
	if err := a.StartContainer(ctx, "db"); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-abc", Namespace: testNamespace,
		Labels: map[string]string{"app": "db", types.LabelManagedBy: types.LabelManagedByValue}}}
	if _, err := client.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- a.StopContainer(ctx, "db") }()
	select {
	case err := <-stopped:
		t.Fatalf("stop returned (%v) while the container's pod still runs", err)
	case <-time.After(700 * time.Millisecond):
	}
	// The ReplicaSet deletes the pod once its process ends.
	if err := client.CoreV1().Pods(testNamespace).Delete(ctx, "db-abc", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return once the pod was gone")
	}
}

// As in Docker, a container docker stop ended reports its process's exit
// code: 137 when it was killed at the end of the stop timeout, not 0.
// docker ps, compose ps and Portainer show it as "Exited (137)".
func TestStopRecordsTheExitCode(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	mustCreate(t, a, RunOptions{Name: "db", Image: "postgres", RestartPolicy: "always"})
	if err := a.StartContainer(ctx, "db"); err != nil {
		t.Fatal(err)
	}
	// The kubelet shows the container killed before the pod goes.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "db-abc", Namespace: testNamespace,
			Labels: map[string]string{"app": "db", types.LabelManagedBy: types.LabelManagedByValue}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}},
		}}},
	}
	if _, err := client.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(700 * time.Millisecond)
		_ = client.CoreV1().Pods(testNamespace).Delete(ctx, "db-abc", metav1.DeleteOptions{})
	}()
	if err := a.StopContainer(ctx, "db"); err != nil {
		t.Fatal(err)
	}
	ctr, err := a.InspectContainer(ctx, "db")
	if err != nil {
		t.Fatal(err)
	}
	if ctr.State.Status != container.StateExited || ctr.State.ExitCode != 137 {
		t.Fatalf("after stop: %s with exit code %d, want exited with 137", ctr.State.Status, ctr.State.ExitCode)
	}

	// A later stop that saw no exit does not report the earlier one's.
	if err := a.StartContainer(ctx, "db"); err != nil {
		t.Fatal(err)
	}
	if err := a.StopContainer(ctx, "db"); err != nil {
		t.Fatal(err)
	}
	if ctr, err = a.InspectContainer(ctx, "db"); err != nil {
		t.Fatal(err)
	}
	if ctr.State.ExitCode != 0 {
		t.Fatalf("after the second stop: exit code %d, want 0, not the first stop's", ctr.State.ExitCode)
	}
}

// As Docker's rm -f does, docker rm kills the container at once rather than
// leaving its pod to its stop grace period, and returns once it is gone:
// its volumes and name are free right after.
func TestRemoveKillsThePodAtOnce(t *testing.T) {
	for _, policy := range []string{"always", "no"} {
		t.Run(policy, func(t *testing.T) {
			a, client := newTestAdapter(t)
			ctx := context.Background()
			grace := int64(30)
			mustCreate(t, a, RunOptions{Name: "db", Image: "postgres", RestartPolicy: policy, StopTimeout: &grace})
			if err := a.StartContainer(ctx, "db"); err != nil {
				t.Fatal(err)
			}
			if policy == "always" {
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-abc-xyz", Namespace: testNamespace,
					Labels: map[string]string{"app": "db", types.LabelManagedBy: types.LabelManagedByValue}}}
				if _, err := client.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			podsGVR := corev1.SchemeGroupVersion.WithResource("pods")
			// The garbage collector deletes the pods of a deleted Deployment
			// with their own grace period.
			client.PrependReactor("delete", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
				list, _ := client.Tracker().List(podsGVR, corev1.SchemeGroupVersion.WithKind("Pod"), testNamespace)
				for _, p := range list.(*corev1.PodList).Items {
					p.DeletionTimestamp, p.DeletionGracePeriodSeconds = ptr(metav1.Now()), &grace
					if err := client.Tracker().Update(podsGVR, &p, testNamespace); err != nil {
						t.Error(err)
					}
				}
				return false, nil, nil
			})
			var graces []*int64
			client.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				del := action.(k8stesting.DeleteAction)
				if obj, err := client.Tracker().Get(podsGVR, testNamespace, del.GetName()); err == nil && obj.(*corev1.Pod).DeletionTimestamp == nil {
					t.Errorf("pod %s deleted before the garbage collector: its ReplicaSet would start another", del.GetName())
				}
				graces = append(graces, del.GetDeleteOptions().GracePeriodSeconds)
				return false, nil, nil
			})

			if err := a.RemoveContainer(ctx, "db"); err != nil {
				t.Fatal(err)
			}
			if pods, _ := client.CoreV1().Pods(testNamespace).List(ctx, podsOf("db")); len(pods.Items) != 0 {
				t.Errorf("docker rm returned with %d pods of the container left", len(pods.Items))
			}
			if len(graces) == 0 {
				t.Fatal("docker rm deleted no pod: it stops with its 30 s grace period")
			}
			for _, g := range graces {
				if g == nil || *g > 1 {
					t.Errorf("pod deleted with grace period %v, want at most 1 s", g)
				}
			}
		})
	}
}

// cancelAwareClient fails Deployment deletes and updates made with a
// cancelled context, as client-go does; the fake clientset ignores the
// context.
type cancelAwareClient struct{ *fake.Clientset }

func (c cancelAwareClient) AppsV1() appsv1client.AppsV1Interface {
	return cancelAwareApps{c.Clientset.AppsV1()}
}

type cancelAwareApps struct{ appsv1client.AppsV1Interface }

func (a cancelAwareApps) Deployments(namespace string) appsv1client.DeploymentInterface {
	return cancelAwareDeployments{a.AppsV1Interface.Deployments(namespace)}
}

type cancelAwareDeployments struct {
	appsv1client.DeploymentInterface
}

func (d cancelAwareDeployments) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.DeploymentInterface.Delete(ctx, name, opts)
}

func (d cancelAwareDeployments) Update(ctx context.Context, dep *appsv1.Deployment, opts metav1.UpdateOptions) (*appsv1.Deployment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return d.DeploymentInterface.Update(ctx, dep, opts)
}

// A client that goes away during docker restart, as Ctrl-C does, does not
// leave the container stopped: as in Docker, the restart completes (D2K-083).
func TestRestartCompletesWhenTheClientGoesAway(t *testing.T) {
	_, client := newTestAdapter(t)
	a := NewWithClient(cancelAwareClient{client}, testNamespace)
	mustCreate(t, a, RunOptions{Name: "db", Image: "postgres", RestartPolicy: "always"})
	if err := a.StartContainer(context.Background(), "db"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if d := action.(k8stesting.UpdateAction).GetObject().(*appsv1.Deployment); *d.Spec.Replicas == 0 {
			cancel()
		}
		return false, nil, nil
	})
	_ = a.RestartContainer(ctx, "db")
	d, err := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "db", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *d.Spec.Replicas != 1 {
		t.Fatalf("replicas = %d after the restart, want 1", *d.Spec.Replicas)
	}
}

// A client that goes away while its container is created, as Ctrl-C on
// docker run or compose up does, leaves no half-created container behind
// to hold the name.
func TestCreateContainerCancelledMidwayLeavesNoDeployment(t *testing.T) {
	_, client := newTestAdapter(t)
	a := NewWithClient(cancelAwareClient{client}, testNamespace)
	ctx, cancel := context.WithCancel(context.Background())
	client.PrependReactor("create", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		cancel()
		return true, nil, context.Canceled
	})
	if _, _, err := a.CreateContainer(ctx, RunOptions{Name: "web", Image: "nginx"}); err == nil {
		t.Fatal("create succeeded, want the Service's error")
	}
	deps, err := client.AppsV1().Deployments(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil || len(deps.Items) != 0 {
		t.Fatalf("Deployments left = %d (%v), want none", len(deps.Items), err)
	}
}

// docker restart records on the container that it is restarting before it
// stops it, and clears that once the container runs again: a d2k that stops
// in between finishes the restart when it starts (D2K-083).
func TestRestartRecordsItIsRestarting(t *testing.T) {
	a, client := newTestAdapter(t)
	mustCreate(t, a, RunOptions{Name: "db", Image: "postgres", RestartPolicy: "always"})
	if err := a.StartContainer(context.Background(), "db"); err != nil {
		t.Fatal(err)
	}
	recorded := false
	client.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		switch d := action.(k8stesting.UpdateAction).GetObject().(*appsv1.Deployment); *d.Spec.Replicas {
		case 0:
			_, recorded = d.Annotations["d2k.portainer.io/restarting"]
		case 1:
			// Its pod runs, so the restart does not wait for it.
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "db-abc", Namespace: testNamespace, Labels: d.Spec.Template.Labels},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}},
			}
			if err := client.Tracker().Add(pod); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Error(err)
			}
		}
		return false, nil, nil
	})
	if err := a.RestartContainer(context.Background(), "db"); err != nil {
		t.Fatal(err)
	}
	if !recorded {
		t.Error("the restart stopped the container without recording it is restarting")
	}
	d, err := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "db", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := d.Annotations["d2k.portainer.io/restarting"]; ok {
		t.Errorf("restarting = %q after the restart, want it cleared", v)
	}
}

// A start whose pod Kubernetes refuses to create, as a full pod quota does,
// fails with Kubernetes' reason, as Docker's start fails when the container
// cannot run (D2K-084).
func TestStartFailsWhenThePodCannotBeCreated(t *testing.T) {
	a, client := newTestAdapter(t)
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", RestartPolicy: "always"})
	d, err := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// What the Deployment controller records once its ReplicaSet fails to
	// create the pod.
	const reason = `pods "web-6d4b9-x2k8p" is forbidden: exceeded quota: pods, requested: pods=1, used: pods=4, limited: pods=4`
	d.Status.Conditions = []appsv1.DeploymentCondition{
		{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse, Reason: "MinimumReplicasUnavailable"},
		{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate", Message: reason},
	}
	if _, err := client.AppsV1().Deployments(testNamespace).UpdateStatus(context.Background(), d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	err = a.StartContainer(context.Background(), "web")
	if err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("StartContainer error = %v, want it to give %q", err, reason)
	}
}

// As Docker's failed start leaves the container stopped, a start that fails
// because Kubernetes refuses to create the pod scales the Deployment back to
// 0: left at 1, the container would start by itself once the quota frees.
func TestFailedStartLeavesTheContainerStopped(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", RestartPolicy: "always"})
	d, err := client.AppsV1().Deployments(testNamespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d.Status.Conditions = []appsv1.DeploymentCondition{
		{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate",
			Message: `pods "web-6d4b9-x2k8p" is forbidden: exceeded quota: pods, requested: pods=1, used: pods=4, limited: pods=4`},
	}
	if _, err := client.AppsV1().Deployments(testNamespace).UpdateStatus(ctx, d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := a.StartContainer(ctx, "web"); err == nil {
		t.Fatal("StartContainer succeeded, want the quota's refusal")
	}
	if d, err = client.AppsV1().Deployments(testNamespace).Get(ctx, "web", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if r := replicasOf(d); r != 0 {
		t.Fatalf("replicas = %d after the failed start, want 0", r)
	}
}

// Docker removes an AutoRemove container whose start fails (moby
// daemon/start.go:119-127), the same as one whose run ends: a --rm
// container is gone either way. d2k's failed start only scaled the
// Deployment back to 0, leaving the container behind as Exited.
func TestFailedStartOfAnAutoRemoveContainerRemovesIt(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", AutoRemove: true})
	d, err := client.AppsV1().Deployments(testNamespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d.Status.Conditions = []appsv1.DeploymentCondition{
		{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate",
			Message: `pods "web-6d4b9-x2k8p" is forbidden: exceeded quota: pods, requested: pods=1, used: pods=4, limited: pods=4`},
	}
	if _, err := client.AppsV1().Deployments(testNamespace).UpdateStatus(ctx, d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := a.StartContainer(ctx, "web"); err == nil {
		t.Fatal("StartContainer succeeded, want the quota's refusal")
	}
	if _, err := a.InspectContainer(ctx, "web"); !errdefs.IsNotFound(err) {
		t.Fatalf("inspect after the failed start of an AutoRemove container = %v, want not found", err)
	}
}
