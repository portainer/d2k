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
