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
	"os"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
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
	client        kubernetes.Interface
	metricsClient *metricsclient.Clientset

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

	// nfsStorageClasses caches nfs.csi.k8s.io StorageClass names discovered at
	// startup, keyed by NFS server address. Used by SwarmCreateService to create
	// NFS-backed PVCs inline when docker stack deploy doesn't call volume create.
	nfsStorageClasses map[string]string // server -> storageClassName
	nfsMu             sync.Mutex        // guards nfsStorageClasses

	// clusterDNS and clusterDomain are the cluster DNS server and domain, read
	// from d2k's own resolv.conf. Pods of compose projects use them to resolve
	// service names within their project first. Empty outside a cluster.
	clusterDNS    string
	clusterDomain string

	// events records the namespace's Docker events (RecordEvents).
	events eventHub

	// restarting holds the Deployment names of the containers being
	// restarted, whose run's end does not remove them.
	restarting sync.Map
}

// Options configures a new KubernetesDockerAdapter.
type Options struct {
	Config *config.Config
}

// NewKubernetesDockerAdapter creates and returns a new KubernetesDockerAdapter.
// It builds a Kubernetes client from either the supplied kubeconfig path or,
// if that is empty, from the in-cluster service account credentials.
func NewKubernetesDockerAdapter(opts *Options) (*KubernetesDockerAdapter, error) {
	restCfg, err := buildRestConfig(opts.Config.KubeConfigPath)
	if err != nil {
		return nil, fmt.Errorf("unable to build Kubernetes REST config: %w", err)
	}

	// client-go's defaults (5 QPS, burst 10) suit a small controller, not a
	// server answering many Docker clients at once.
	restCfg.QPS, restCfg.Burst = 50, 100

	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("unable to create Kubernetes client: %w", err)
	}

	// Verify connectivity.
	if _, err := client.CoreV1().Namespaces().Get(context.Background(), opts.Config.Namespace, metav1.GetOptions{}); err != nil {
		return nil, fmt.Errorf("unable to reach namespace %q in cluster: %w", opts.Config.Namespace, err)
	}

	mc := initMetricsClient(restCfg)

	clusterDNS, clusterDomain := readClusterDNS("/etc/resolv.conf")
	if clusterDNS == "" || clusterDomain == "" {
		log.Warn().Str("context", "KubernetesAdapter").Msg("Cluster DNS not found in /etc/resolv.conf — compose service names will not be scoped to their project")
	}

	a := NewWithClient(client, opts.Config.Namespace)
	a.metricsClient = mc
	a.restConfig = restCfg
	a.apiServerHost = apiServerHost(restCfg.Host)
	a.lowPortThreshold = opts.Config.LowPortThreshold
	a.gpuResourceName = opts.Config.GPUResourceName
	a.clusterDNS = clusterDNS
	a.clusterDomain = clusterDomain
	return a, nil
}

// NewWithClient returns an adapter that uses client for the given namespace,
// without contacting the cluster. Features that need more than the client
// (exec, metrics, GPU) stay disabled.
func NewWithClient(client kubernetes.Interface, namespace string) *KubernetesDockerAdapter {
	return &KubernetesDockerAdapter{
		client:            client,
		namespace:         namespace,
		nfsStorageClasses: map[string]string{},
	}
}

// readClusterDNS returns the nameserver and cluster domain from a pod's
// resolv.conf, whose search list contains "svc.<cluster domain>".
func readClusterDNS(path string) (nameserver, domain string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "nameserver":
			if nameserver == "" {
				nameserver = fields[1]
			}
		case "search":
			for _, s := range fields[1:] {
				if d, ok := strings.CutPrefix(s, "svc."); ok && domain == "" {
					domain = d
				}
			}
		}
	}
	return nameserver, domain
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
