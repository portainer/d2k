package adapter

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
