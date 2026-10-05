package adapter

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

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
