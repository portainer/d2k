package adapter

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInjectQuotaDefaultsNoQuota(t *testing.T) {
	a, _ := newTestAdapter(t)
	got, err := a.injectQuotaDefaults(context.Background(), corev1.ResourceRequirements{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != nil || got.Limits != nil {
		t.Fatalf("reqs = %+v, want untouched with no ResourceQuota in the namespace", got)
	}
}

func TestInjectQuotaDefaultsRequests(t *testing.T) {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "compute", Namespace: testNamespace},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceRequestsCPU:    resource.MustParse("4"),
			corev1.ResourceRequestsMemory: resource.MustParse("8Gi"),
		}},
	}
	a, _ := newTestAdapter(t, quota)

	got, err := a.injectQuotaDefaults(context.Background(), corev1.ResourceRequirements{})
	if err != nil {
		t.Fatal(err)
	}
	cpu, mem := got.Requests[corev1.ResourceCPU], got.Requests[corev1.ResourceMemory]
	if cpu.String() != "10m" || mem.String() != "32Mi" {
		t.Fatalf("requests = %+v, want 10m cpu and 32Mi memory injected", got.Requests)
	}

	// A value the caller set is never overwritten.
	preset := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}}
	got, err = a.injectQuotaDefaults(context.Background(), preset)
	if err != nil {
		t.Fatal(err)
	}
	if cpu := got.Requests[corev1.ResourceCPU]; cpu.String() != "500m" {
		t.Fatalf("requests.cpu = %s, want the caller's 500m kept", cpu.String())
	}
}

// A ResourceQuota enforcing limits.cpu/limits.memory, without requests.*, is
// the common way to cap usage without reserving it. A container created
// without explicit limits is rejected 403 by the quota admission controller,
// exactly as one without requests is when the quota enforces requests.*.
func TestInjectQuotaDefaultsLimits(t *testing.T) {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "compute", Namespace: testNamespace},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceLimitsCPU:    resource.MustParse("4"),
			corev1.ResourceLimitsMemory: resource.MustParse("8Gi"),
		}},
	}
	a, _ := newTestAdapter(t, quota)

	got, err := a.injectQuotaDefaults(context.Background(), corev1.ResourceRequirements{})
	if err != nil {
		t.Fatal(err)
	}
	cpu, mem := got.Limits[corev1.ResourceCPU], got.Limits[corev1.ResourceMemory]
	if cpu.String() != "10m" || mem.String() != "32Mi" {
		t.Fatalf("limits = %+v, want defaults injected so the pod isn't rejected by the quota", got.Limits)
	}

	// A limit the caller set is never overwritten.
	preset := corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}}
	got, err = a.injectQuotaDefaults(context.Background(), preset)
	if err != nil {
		t.Fatal(err)
	}
	if mem := got.Limits[corev1.ResourceMemory]; mem.String() != "256Mi" {
		t.Fatalf("limits.memory = %s, want the caller's 256Mi kept", mem.String())
	}
}

// A quota enforcing both requests.cpu and limits.cpu, with the caller asking
// for more CPU than the default but setting no limit, must inject a limit at
// least as large as that request: Kubernetes rejects a pod whose limit is
// below its request.
func TestInjectQuotaDefaultsLimitAtLeastRequest(t *testing.T) {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "compute", Namespace: testNamespace},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceRequestsCPU: resource.MustParse("4"),
			corev1.ResourceLimitsCPU:   resource.MustParse("4"),
		}},
	}
	a, _ := newTestAdapter(t, quota)

	reqs := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}}
	got, err := a.injectQuotaDefaults(context.Background(), reqs)
	if err != nil {
		t.Fatal(err)
	}
	if limit := got.Limits[corev1.ResourceCPU]; limit.Cmp(resource.MustParse("500m")) < 0 {
		t.Fatalf("limits.cpu = %s, want at least the 500m request", limit.String())
	}
}
