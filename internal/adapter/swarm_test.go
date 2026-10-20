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
