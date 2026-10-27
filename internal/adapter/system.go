package adapter

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HostID returns an identifier unique to this d2k installation: the UID of
// its namespace. Docker clients and Portainer key per-host state on it.
func (a *KubernetesDockerAdapter) HostID(ctx context.Context) (string, error) {
	ns, err := a.client.CoreV1().Namespaces().Get(ctx, a.namespace, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("unable to get namespace %q: %w", a.namespace, err)
	}
	return string(ns.UID), nil
}

// HostResources returns the CPUs and memory (bytes) to report as the
// host's: the cluster's allocatable capacity, capped by the namespace's
// ResourceQuotas, which bound what its workloads can use.
func (a *KubernetesDockerAdapter) HostResources(ctx context.Context) (int, int64, error) {
	nodes, err := a.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, fmt.Errorf("unable to list nodes: %w", err)
	}
	cpu, memory := resource.Quantity{}, resource.Quantity{}
	for _, n := range nodes.Items {
		cpu.Add(n.Status.Allocatable[corev1.ResourceCPU])
		memory.Add(n.Status.Allocatable[corev1.ResourceMemory])
	}

	quotas, err := a.client.CoreV1().ResourceQuotas(a.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, fmt.Errorf("unable to list resource quotas: %w", err)
	}
	for _, q := range quotas.Items {
		if limit, ok := quotaLimit(q.Spec.Hard, corev1.ResourceLimitsCPU, corev1.ResourceRequestsCPU, corev1.ResourceCPU); ok && limit.Cmp(cpu) < 0 {
			cpu = limit
		}
		if limit, ok := quotaLimit(q.Spec.Hard, corev1.ResourceLimitsMemory, corev1.ResourceRequestsMemory, corev1.ResourceMemory); ok && limit.Cmp(memory) < 0 {
			memory = limit
		}
	}
	// Value rounds up; report the whole CPUs workloads can rely on, but at
	// least 1 when there is any CPU: a quota below one CPU still runs
	// containers, and clients divide by NCPU.
	ncpu := int(cpu.MilliValue() / 1000)
	if ncpu == 0 && cpu.MilliValue() > 0 {
		ncpu = 1
	}
	return ncpu, memory.Value(), nil
}

// quotaLimit returns the first of names set in hard. Limits come before
// requests: a namespace can burst up to its limits.
func quotaLimit(hard corev1.ResourceList, names ...corev1.ResourceName) (resource.Quantity, bool) {
	for _, name := range names {
		if q, ok := hard[name]; ok {
			return q, true
		}
	}
	return resource.Quantity{}, false
}
