package adapter

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

// A LimitRange sets its default limits on the pod, not on the Deployment's
// template: the limit docker stats reports is the pod's.
func TestContainerUsageMemoryLimitFromPod(t *testing.T) {
	d := deployment("web", "uid-web", managed())
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "web"}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("16Gi"),
	}}}
	labels := managed()
	labels["app"] = "web"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: testNamespace, Labels: labels},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	a, _ := newTestAdapter(t, d, node, pod)

	u, err := a.ContainerUsage(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(256 << 20); u.MemoryLimit != want {
		t.Errorf("MemoryLimit = %d, want the pod's limit %d", u.MemoryLimit, want)
	}
}

// Docker host mode needs no cluster-scoped access, as README's RBAC section
// says: without the node-reader ClusterRole, docker stats still reports the
// container's usage, against its own limit.
func TestContainerUsageWithoutNodeAccess(t *testing.T) {
	d := deployment("web", "uid-web", managed())
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "web", Resources: corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
	}}}
	a, client := newTestAdapter(t, d)
	client.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("RBAC: access denied"))
	})

	u, err := a.ContainerUsage(context.Background(), "web")
	if err != nil {
		t.Fatalf("ContainerUsage without node access: %v", err)
	}
	if want := int64(256 << 20); u.MemoryLimit != want {
		t.Errorf("MemoryLimit = %d, want %d", u.MemoryLimit, want)
	}
}

// docker stats streams a sample per container every second, and Portainer
// shows several containers' stats at once. The host's CPUs and memory come
// from cluster-wide lists that change rarely: each sample must not list
// every Node and ResourceQuota again.
func TestContainerUsageDoesNotListTheClusterPerSample(t *testing.T) {
	var objs []runtime.Object
	for _, name := range []string{"web", "db", "cache"} {
		d := deployment(name, "uid-"+name, managed())
		d.Spec.Template.Spec.Containers = []corev1.Container{{Name: name}}
		objs = append(objs, d)
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("16Gi"),
	}}}
	a, client := newTestAdapter(t, append(objs, node)...)
	lists := map[string]int{}
	for _, resource := range []string{"nodes", "resourcequotas"} {
		client.PrependReactor("list", resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
			lists[action.GetResource().Resource]++
			return false, nil, nil
		})
	}

	for range 5 {
		for _, name := range []string{"web", "db", "cache"} {
			u, err := a.ContainerUsage(context.Background(), name)
			if err != nil {
				t.Fatal(err)
			}
			if u.OnlineCPUs != 4 || u.MemoryLimit != 16<<30 {
				t.Fatalf("usage = %+v, want the node's 4 CPUs and 16Gi", u)
			}
		}
	}
	if lists["nodes"] != 1 || lists["resourcequotas"] != 1 {
		t.Errorf("15 samples listed nodes %d and resource quotas %d times, want once each", lists["nodes"], lists["resourcequotas"])
	}
}
