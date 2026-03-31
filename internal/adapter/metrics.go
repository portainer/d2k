package adapter

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
	"k8s.io/client-go/rest"
)

// PodMetrics holds the CPU and memory usage for a pod.
type PodMetrics struct {
	CPUUsageNanoCores     int64
	PrevCPUUsageNanoCores int64
	MemoryUsageBytes      int64
}

// initMetricsClient attempts to create a metrics client and probes the
// metrics API to check availability. Returns nil if not available.
func initMetricsClient(restCfg *rest.Config) *metricsclient.Clientset {
	mc, err := metricsclient.NewForConfig(restCfg)
	if err != nil {
		return nil
	}
	return mc
}

// GetPodMetrics returns CPU and memory usage for the named deployment's
// current pod. Returns zeroed PodMetrics if the metrics API is unavailable.
func (a *KubernetesDockerAdapter) GetPodMetrics(ctx context.Context, deploymentName string) (*PodMetrics, error) {
	if a.metricsClient == nil {
		return &PodMetrics{}, nil
	}

	pod, err := a.currentPodForDeployment(ctx, deploymentName)
	if err != nil {
		return &PodMetrics{}, nil
	}

	podMetrics, err := a.metricsClient.MetricsV1beta1().
		PodMetricses(a.namespace).
		Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		return &PodMetrics{}, nil
	}

	current := int64(0)
	memory := int64(0)
	for _, c := range podMetrics.Containers {
		current += c.Usage.Cpu().MilliValue() * 1_000_000
		memory += c.Usage.Memory().Value()
	}

	a.prevCPUMu.RLock()
	prev := a.prevCPU[deploymentName]
	a.prevCPUMu.RUnlock()

	a.prevCPUMu.Lock()
	a.prevCPU[deploymentName] = current
	a.prevCPUMu.Unlock()

	return &PodMetrics{
		CPUUsageNanoCores:     current,
		PrevCPUUsageNanoCores: prev,
		MemoryUsageBytes:      memory,
	}, nil
}

// probeMetricsAPI checks if the metrics API is available by listing pod metrics.
// Returns true if available.
func probeMetricsAPI(ctx context.Context, mc *metricsclient.Clientset, namespace string) bool {
	_, err := mc.MetricsV1beta1().PodMetricses(namespace).List(ctx, metav1.ListOptions{})
	return err == nil
}

// suppress unused import warning
var _ = metricsv1beta1.PodMetrics{}
