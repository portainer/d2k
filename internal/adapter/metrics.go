package adapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

// Usage is a container's current resource use, as Kubernetes measures it.
type Usage struct {
	CPUNanoCores int64 // CPU in use, in billionths of a CPU
	MemoryBytes  int64 // working set
	MemoryLimit  int64 // the container's limit (its pod's, else its template's), else the host's memory
	OnlineCPUs   int   // the host's CPUs
}

// initMetricsClient creates a client for the metrics API (metrics-server).
// Whether the API is served is checked per request: it may be installed
// after d2k starts.
func initMetricsClient(restCfg *rest.Config) *metricsclient.Clientset {
	mc, err := metricsclient.NewForConfig(restCfg)
	if err != nil {
		return nil
	}
	return mc
}

// ContainerUsage reads the container's CPU and memory use from the metrics
// API. Without the metrics API, or before its first sample, use is zero.
func (a *KubernetesDockerAdapter) ContainerUsage(ctx context.Context, name string) (Usage, error) {
	resolved, err := a.resolveDeploymentName(ctx, name)
	if err != nil {
		return Usage{}, err
	}
	d, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, resolved, metav1.GetOptions{})
	if err != nil {
		return Usage{}, fmt.Errorf("unable to get deployment %q: %w", resolved, err)
	}
	// docker stats answers from the container's own limit, as reported
	// below, when host-wide node access isn't granted; see /info's Info.
	ncpu, hostMemory, err := a.HostResources(ctx)
	if err != nil {
		log.Warn().Str("context", "KubernetesAdapter").Err(err).Msg("ContainerUsage: unable to read host resources")
	}
	u := Usage{OnlineCPUs: max(ncpu, 1), MemoryLimit: hostMemory}
	if c := d.Spec.Template.Spec.Containers; len(c) > 0 {
		if limit, ok := c[0].Resources.Limits[corev1.ResourceMemory]; ok {
			u.MemoryLimit = limit.Value()
		}
	}

	pod, err := a.currentPodForDeployment(ctx, resolved)
	if err == nil {
		// A LimitRange's default limit is set on the pod only.
		if c := pod.Spec.Containers; len(c) > 0 {
			if limit, ok := c[0].Resources.Limits[corev1.ResourceMemory]; ok {
				u.MemoryLimit = limit.Value()
			}
		}
	}
	if errors.Is(err, errNoPod) || a.metricsClient == nil {
		return u, nil
	}
	if err != nil {
		return Usage{}, err
	}
	metrics, err := a.metricsClient.MetricsV1beta1().PodMetricses(a.namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		log.Debug().Str("context", "KubernetesAdapter").Str("pod", pod.Name).Err(err).Msg("No metrics for pod")
		return u, nil
	}
	for _, c := range metrics.Containers {
		u.CPUNanoCores += c.Usage.Cpu().ScaledValue(resource.Nano)
		u.MemoryBytes += c.Usage.Memory().Value()
	}
	return u, nil
}
