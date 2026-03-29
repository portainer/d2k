// Package adapter is the core of d2k. It holds a Kubernetes client scoped to a
// single namespace and exposes methods that correspond directly to Docker API
// operations. Each method translates the incoming Docker semantics into one or
// more Kubernetes API calls, then converts the result back into the Docker wire
// format expected by the caller.
package adapter

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/portainer/d2k/internal/config"
)

// KubernetesDockerAdapter bridges Docker API calls to a single Kubernetes namespace.
type KubernetesDockerAdapter struct {
	// client is the Kubernetes API client.
	client kubernetes.Interface

	// namespace is the target Kubernetes namespace for all operations.
	namespace string

	// lowPortThreshold is the port number below which explicit -p mappings result
	// in a LoadBalancer Service instead of a NodePort Service.
	lowPortThreshold int

	logger *zap.SugaredLogger
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

	return &KubernetesDockerAdapter{
		client:           client,
		namespace:        opts.Config.Namespace,
		lowPortThreshold: opts.Config.LowPortThreshold,
		logger:           opts.Logger,
	}, nil
}

// buildRestConfig returns a *rest.Config from a kubeconfig file path, or falls
// back to in-cluster config when path is empty.
func buildRestConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}

	return rest.InClusterConfig()
}
