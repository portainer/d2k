// Package adapter is the core of d2k. It holds a Kubernetes client scoped to a
// single namespace and exposes methods that correspond directly to Docker API
// operations. Each method translates the incoming Docker semantics into one or
// more Kubernetes API calls, then converts the result back into the Docker wire
// format expected by the caller.
package adapter

import (
	"context"
	"fmt"
	"net/url"
	"sync"

	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/portainer/d2k/internal/config"
)

// KubernetesDockerAdapter bridges Docker API calls to a single Kubernetes namespace.
type KubernetesDockerAdapter struct {
	// client is the Kubernetes API client.
	client kubernetes.Interface
	metricsClient    *metricsclient.Clientset

	// namespace is the target Kubernetes namespace for all operations.
	namespace string

	// apiServerHost is the hostname or IP of the Kubernetes API server derived
	// from the REST config. Used to identify the leader control-plane node:
	// the control-plane node whose internal IP matches this host is reported
	// as the Swarm leader.
	apiServerHost string

	// lowPortThreshold is the port number below which explicit -p mappings result
	// in a LoadBalancer Service instead of a NodePort Service.
	lowPortThreshold int

	// restConfig is the Kubernetes REST config used to build the client.
	// Stored so exec operations can reuse it without rebuilding from disk each time.
	restConfig *rest.Config

	// gpuResourceName is the Kubernetes resource name for GPU device requests
	// (e.g. "nvidia.com/gpu" or "amd.com/gpu"). Empty means GPU support is disabled.
	gpuResourceName string

	logger *zap.SugaredLogger
	prevCPU   map[string]int64
	prevCPUMu sync.RWMutex
	networks   map[string]*NetworkSummary
	networksMu sync.RWMutex
	// nfsVolumes stores NFS driver opts keyed by sanitised volume name so
	// SwarmCreateService can inject inline NFS pod volumes without needing a PV.
	nfsVolumes   map[string]nfsVolumeConfig
	nfsVolumesMu sync.RWMutex
}

// Options configures a new KubernetesDockerAdapter.
type Options struct {
	Config *config.Config
	Logger *zap.SugaredLogger
}

// NewKubernetesDockerAdapter creates and returns a new KubernetesDockerAdapter.
// It builds a Kubernetes client from either the supplied kubeconfig path or,
// if that is empty, from the in-cluster service account credentials.
func NewKubernetesDockerAdapter(opts *Options) (*KubernetesDockerAdapter, error) {
	restCfg, err := buildRestConfig(opts.Config.KubeConfigPath)
	if err != nil {
		return nil, fmt.Errorf("unable to build Kubernetes REST config: %w", err)
	}

	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("unable to create Kubernetes client: %w", err)
	}

	// Verify connectivity.
	if _, err := client.CoreV1().Namespaces().Get(context.Background(), opts.Config.Namespace, metav1.GetOptions{}); err != nil {
		return nil, fmt.Errorf("unable to reach namespace %q in cluster: %w", opts.Config.Namespace, err)
	}

// Probe metrics API — optional, failures are non-fatal.
	mc := initMetricsClient(restCfg)
	if mc != nil {
		if probeMetricsAPI(context.Background(), mc, opts.Config.Namespace) {
			opts.Logger.Infow("metrics API available — stats will show real data")
		} else {
			opts.Logger.Infow("metrics API not available — stats will return zeroes")
			mc = nil
		}
	}

	return &KubernetesDockerAdapter{
		client:           client,
		metricsClient:    mc,
		restConfig:       restCfg,
		namespace:        opts.Config.Namespace,
		apiServerHost:    apiServerHost(restCfg.Host),
		lowPortThreshold: opts.Config.LowPortThreshold,
		gpuResourceName:  opts.Config.GPUResourceName,
		logger:           opts.Logger,
		prevCPU:          map[string]int64{},
		networks:         map[string]*NetworkSummary{},
		nfsVolumes:       map[string]nfsVolumeConfig{},
	}, nil
}

// apiServerHost extracts the bare hostname or IP from a Kubernetes API server
// URL (e.g. "https://10.0.0.1:6443" -> "10.0.0.1").
func apiServerHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	host := u.Hostname() // strips port
	return host
}

// buildRestConfig returns a *rest.Config from a kubeconfig file path, or falls
// back to in-cluster config when path is empty.
func buildRestConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}

	return rest.InClusterConfig()
}
