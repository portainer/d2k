package adapter

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/internal/filters"
	"github.com/portainer/d2k/internal/types"
)

func swarmObjectMeta(name, uid, stack string) metav1.ObjectMeta {
	labels := map[string]string{
		types.LabelManagedBy:      types.LabelManagedByValue,
		types.LabelSwarmManagedBy: types.LabelSwarmManagedByValue,
	}
	if stack != "" {
		labels["com.docker.stack.namespace"] = stack
	}
	return metav1.ObjectMeta{
		Name:        name,
		Namespace:   testNamespace,
		UID:         k8stypes.UID(uid),
		Labels:      labels,
		Annotations: map[string]string{"d2k.portainer.io/swarm-name": name},
	}
}

func specNames(objs []map[string]any) []string {
	var names []string
	for _, o := range objs {
		names = append(names, o["Spec"].(map[string]any)["Name"].(string))
	}
	return names
}

func TestSwarmListSecretsFilters(t *testing.T) {
	a, _ := newTestAdapter(t,
		&corev1.Secret{ObjectMeta: swarmObjectMeta("app-db-password", "11111111-0000-0000-0000-000000000000", "app")},
		&corev1.Secret{ObjectMeta: swarmObjectMeta("other-db-password", "22222222-0000-0000-0000-000000000000", "other")},
	)

	tests := []struct {
		name string
		args filters.Args
		want []string
	}{
		{name: "no filters", args: filters.NewArgs(), want: []string{"app-db-password", "other-db-password"}},
		// docker stack rm app must only see the app stack's secrets.
		{name: "stack label", args: filters.NewArgs(filters.Arg("label", "com.docker.stack.namespace=app")), want: []string{"app-db-password"}},
		{name: "name prefix", args: filters.NewArgs(filters.Arg("name", "other")), want: []string{"other-db-password"}},
		{name: "names exact", args: filters.NewArgs(filters.Arg("names", "other")), want: nil},
		{name: "id prefix", args: filters.NewArgs(filters.Arg("id", "2222")), want: []string{"other-db-password"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := a.SwarmListSecrets(context.Background(), tt.args)
			if err != nil {
				t.Fatalf("SwarmListSecrets() error: %v", err)
			}
			if names := specNames(got); !slices.Equal(names, tt.want) {
				t.Fatalf("SwarmListSecrets() = %v, want %v", names, tt.want)
			}
		})
	}
}

func TestSwarmListConfigsFilters(t *testing.T) {
	a, _ := newTestAdapter(t,
		&corev1.ConfigMap{ObjectMeta: swarmObjectMeta("app-nginx-conf", "33333333-0000-0000-0000-000000000000", "app")},
		&corev1.ConfigMap{ObjectMeta: swarmObjectMeta("other-nginx-conf", "44444444-0000-0000-0000-000000000000", "other")},
	)

	got, err := a.SwarmListConfigs(context.Background(), filters.NewArgs(filters.Arg("label", "com.docker.stack.namespace=app")))
	if err != nil {
		t.Fatalf("SwarmListConfigs() error: %v", err)
	}
	if names := specNames(got); !slices.Equal(names, []string{"app-nginx-conf"}) {
		t.Fatalf("SwarmListConfigs() = %v, want [app-nginx-conf]", names)
	}
}

func TestSwarmCreateServiceNameConflict(t *testing.T) {
	const body = `{"Name":"%s","TaskTemplate":{"ContainerSpec":{"Image":"nginx:1.27"}}}`

	t.Run("unmanaged deployment is not overwritten", func(t *testing.T) {
		a, client := newTestAdapter(t, deployment("d2k", "aaaaaaaa-0000-0000-0000-000000000000", map[string]string{"app": "d2k"}))

		_, err := a.SwarmCreateService(context.Background(), strings.NewReader(fmt.Sprintf(body, "d2k")))
		if !errdefs.IsConflict(err) {
			t.Fatalf("SwarmCreateService() error = %v, want conflict", err)
		}
		d, _ := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "d2k", metav1.GetOptions{})
		if len(d.Spec.Template.Spec.Containers) != 0 {
			t.Fatalf("unmanaged deployment was overwritten: %+v", d.Spec.Template.Spec.Containers)
		}
	})

	t.Run("standalone container is not overwritten", func(t *testing.T) {
		a, _ := newTestAdapter(t, deployment("web", "bbbbbbbb-0000-0000-0000-000000000000", managed()))

		_, err := a.SwarmCreateService(context.Background(), strings.NewReader(fmt.Sprintf(body, "web")))
		if !errdefs.IsConflict(err) {
			t.Fatalf("SwarmCreateService() error = %v, want conflict", err)
		}
	})

	t.Run("existing swarm service is updated in place", func(t *testing.T) {
		a, client := newTestAdapter(t, deployment("web", "cccccccc-0000-0000-0000-000000000000", swarmManaged("web")))

		if _, err := a.SwarmCreateService(context.Background(), strings.NewReader(fmt.Sprintf(body, "web"))); err != nil {
			t.Fatalf("SwarmCreateService() error: %v", err)
		}
		d, _ := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "web", metav1.GetOptions{})
		if got := d.Spec.Template.Spec.Containers[0].Image; got != "nginx:1.27" {
			t.Fatalf("image = %q, want nginx:1.27", got)
		}
	})
}

func service(name string, labels map[string]string) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels}}
}

func TestSwarmDeleteServiceRemovesOnlyItsServices(t *testing.T) {
	a, client := newTestAdapter(t,
		deployment("my-d2k", "dddddddd-0000-0000-0000-000000000000", swarmManaged("my-d2k")),
		service("my-d2k", swarmManaged("my-d2k")),
		service("my-d2k-lb", swarmManaged("my-d2k")),
		deployment("app-redis", "eeeeeeee-0000-0000-0000-000000000000", swarmManaged("app-redis")),
		service("redis", swarmManaged("app-redis")),
		// d2k's own API Service, which the old name guess ("my-d2k" -> "d2k") deleted.
		service("d2k", map[string]string{"app": "d2k"}),
	)

	for _, id := range []string{"my-d2k", "app-redis"} {
		if err := a.SwarmDeleteService(context.Background(), id); err != nil {
			t.Fatalf("SwarmDeleteService(%q) error: %v", id, err)
		}
	}

	list, err := client.CoreV1().Services(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var remaining []string
	for _, s := range list.Items {
		remaining = append(remaining, s.Name)
	}
	if !slices.Equal(remaining, []string{"d2k"}) {
		t.Fatalf("remaining Services = %v, want [d2k]", remaining)
	}
}

func TestSwarmCreateServiceDNSNameCollision(t *testing.T) {
	const otherRedis = `{"Name":"other_redis","Labels":{"com.docker.stack.namespace":"other"},"TaskTemplate":{"ContainerSpec":{"Image":"redis:7"}}}`

	t.Run("name owned by another live service", func(t *testing.T) {
		a, client := newTestAdapter(t,
			deployment("app-redis", "11111111-aaaa-0000-0000-000000000000", swarmManaged("app-redis")),
			service("redis", swarmManaged("app-redis")),
		)

		_, err := a.SwarmCreateService(context.Background(), strings.NewReader(otherRedis))
		if !errdefs.IsConflict(err) {
			t.Fatalf("SwarmCreateService() error = %v, want conflict", err)
		}
		svc, _ := client.CoreV1().Services(testNamespace).Get(context.Background(), "redis", metav1.GetOptions{})
		if owner := svc.Labels[types.LabelSwarmService]; owner != "app-redis" {
			t.Fatalf("redis Service owner = %q, want app-redis", owner)
		}
		if _, err := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "other-redis", metav1.GetOptions{}); err == nil {
			t.Fatal("other-redis Deployment was not rolled back")
		}
	})

	t.Run("stale name from a deleted service is reclaimed", func(t *testing.T) {
		a, client := newTestAdapter(t, service("redis", swarmManaged("gone-redis")))

		if _, err := a.SwarmCreateService(context.Background(), strings.NewReader(otherRedis)); err != nil {
			t.Fatalf("SwarmCreateService() error: %v", err)
		}
		svc, _ := client.CoreV1().Services(testNamespace).Get(context.Background(), "redis", metav1.GetOptions{})
		if owner := svc.Labels[types.LabelSwarmService]; owner != "other-redis" {
			t.Fatalf("redis Service owner = %q, want other-redis", owner)
		}
	})

	t.Run("name used by an unmanaged Service", func(t *testing.T) {
		a, client := newTestAdapter(t, service("redis", map[string]string{"app": "redis"}))

		_, err := a.SwarmCreateService(context.Background(), strings.NewReader(otherRedis))
		if !errdefs.IsConflict(err) {
			t.Fatalf("SwarmCreateService() error = %v, want conflict", err)
		}
		svc, _ := client.CoreV1().Services(testNamespace).Get(context.Background(), "redis", metav1.GetOptions{})
		if svc.Labels["app"] != "redis" {
			t.Fatalf("unmanaged Service was modified: %v", svc.Labels)
		}
	})
}

func TestSwarmCreateServiceSecretMustBeManaged(t *testing.T) {
	const body = `{"Name":"web","TaskTemplate":{"ContainerSpec":{"Image":"nginx:1.27","Secrets":[{"SecretName":"%s","File":{"Name":"secret"}}]}}}`

	a, client := newTestAdapter(t,
		// d2k's own serving certificate: never mountable through the Docker API.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "d2k-tls", Namespace: testNamespace}},
		&corev1.Secret{ObjectMeta: swarmObjectMeta("app-db-password", "55555555-0000-0000-0000-000000000000", "app")},
	)

	_, err := a.SwarmCreateService(context.Background(), strings.NewReader(fmt.Sprintf(body, "d2k-tls")))
	if !errdefs.IsInvalidParameter(err) {
		t.Fatalf("SwarmCreateService() with unmanaged secret error = %v, want invalid parameter", err)
	}
	if _, err := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "web", metav1.GetOptions{}); err == nil {
		t.Fatal("Deployment was created for a service mounting an unmanaged secret")
	}

	if _, err := a.SwarmCreateService(context.Background(), strings.NewReader(fmt.Sprintf(body, "app_db_password"))); err != nil {
		t.Fatalf("SwarmCreateService() with managed secret error: %v", err)
	}
}

func TestSwarmCreateServiceConfigMustBeManaged(t *testing.T) {
	const body = `{"Name":"web","TaskTemplate":{"ContainerSpec":{"Image":"nginx:1.27","Configs":[{"ConfigName":"%s","File":{"Name":"config"}}]}}}`

	a, client := newTestAdapter(t,
		// An admin's ConfigMap in d2k's namespace: not a Swarm config.
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: testNamespace}, Data: map[string]string{"settings": "theirs"}},
		&corev1.ConfigMap{ObjectMeta: swarmObjectMeta("app-nginx-conf", "66666666-0000-0000-0000-000000000000", "app")},
	)

	_, err := a.SwarmCreateService(context.Background(), strings.NewReader(fmt.Sprintf(body, "settings")))
	if !errdefs.IsInvalidParameter(err) {
		t.Fatalf("SwarmCreateService() with unmanaged config error = %v, want invalid parameter", err)
	}
	if _, err := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "web", metav1.GetOptions{}); err == nil {
		t.Fatal("Deployment was created for a service mounting an unmanaged config")
	}

	if _, err := a.SwarmCreateService(context.Background(), strings.NewReader(fmt.Sprintf(body, "app_nginx_conf"))); err != nil {
		t.Fatalf("SwarmCreateService() with managed config error: %v", err)
	}
}

// The container of a Swarm service carries the labels Docker gives a task's
// container, and its tasks name it: clients such as Portainer find the
// service, and the access to it, of a container by them.
func TestSwarmServiceContainerIsTheTasksContainer(t *testing.T) {
	ctx := context.Background()
	a, client := newTestAdapter(t)

	created, err := a.SwarmCreateService(ctx, strings.NewReader(`{"Name":"web","Labels":{"com.docker.stack.namespace":"shop"},"TaskTemplate":{"ContainerSpec":{"Image":"nginx:1.27"}}}`))
	if err != nil {
		t.Fatalf("SwarmCreateService() error: %v", err)
	}
	serviceID := created["ID"].(string)

	containers, err := a.ListContainers(ctx, true)
	if err != nil || len(containers) != 1 {
		t.Fatalf("ListContainers() = %v, %v; want the service's container", containers, err)
	}
	labels := containers[0].Labels
	for key, want := range map[string]string{
		"com.docker.swarm.service.id":   serviceID,
		"com.docker.swarm.service.name": "web",
		"com.docker.stack.namespace":    "shop",
	} {
		if labels[key] != want {
			t.Errorf("label %s = %q, want %q", key, labels[key], want)
		}
	}

	inspected, err := a.InspectContainer(ctx, "web")
	if err != nil {
		t.Fatalf("InspectContainer() error: %v", err)
	}
	if got := inspected.Config.Labels["com.docker.swarm.service.id"]; got != serviceID {
		t.Errorf("inspected service ID label = %q, want %q", got, serviceID)
	}

	// A running pod of the service: its task names the service's container.
	d, err := client.AppsV1().Deployments(testNamespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: testNamespace, UID: "dddddddd-0000-0000-0000-000000000000", Labels: d.Spec.Template.Labels},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "web", ContainerID: "containerd://0123456789abcdef"}},
		},
	}
	if _, err := client.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	tasks, err := a.SwarmListTasks(ctx, "", "")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("SwarmListTasks() = %v, %v; want one task", tasks, err)
	}
	status := tasks[0]["Status"].(map[string]any)["ContainerStatus"].(map[string]any)
	if status["ContainerID"] != containers[0].ID {
		t.Errorf("task container ID = %v, want the service's container %s", status["ContainerID"], containers[0].ID)
	}

	task, err := a.SwarmInspectTask(ctx, tasks[0]["ID"].(string))
	if err != nil {
		t.Fatalf("SwarmInspectTask() error: %v", err)
	}
	if task["ServiceID"] != serviceID {
		t.Errorf("inspected task service = %v, want %s", task["ServiceID"], serviceID)
	}
	if got := task["Status"].(map[string]any)["ContainerStatus"].(map[string]any)["ContainerID"]; got != containers[0].ID {
		t.Errorf("inspected task container ID = %v, want %s", got, containers[0].ID)
	}
}

// Volume names that differ only by case, _ or - map to the same PVC. A
// service doesn't mount another volume's PVC: it is refused, as docker
// volume create is. The PVC a service creates for its volume records the
// volume's name, so the other names don't find it either.
func TestServiceVolumeNamesSharingAPVCName(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()
	if _, err := a.CreateVolume(ctx, CreateVolumeOptions{Name: "demo-data"}); err != nil {
		t.Fatal(err)
	}
	service := func(name, volume string) string {
		return `{"Name":"` + name + `","TaskTemplate":{"ContainerSpec":{"Image":"nginx","Mounts":[{"Type":"volume","Source":"` + volume + `","Target":"/data"}]}}}`
	}
	if _, err := a.SwarmCreateService(ctx, strings.NewReader(service("web", "Demo_Data"))); !errdefs.IsConflict(err) {
		t.Errorf("service mounting another name for demo-data's PVC = %v, want conflict", err)
	}

	if _, err := a.SwarmCreateService(ctx, strings.NewReader(service("db", "db_data"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.InspectVolume(ctx, "db_data"); err != nil {
		t.Errorf("inspect of the service's volume = %v", err)
	}
	if _, err := a.InspectVolume(ctx, "db-data"); !errdefs.IsNotFound(err) {
		t.Errorf("inspect of another name for the service's volume = %v, want not found", err)
	}
}
