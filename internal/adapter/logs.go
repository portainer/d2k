package adapter

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/portainer/d2k/internal/types"
)

// LogOptions mirrors the subset of docker logs flags d2k supports.
type LogOptions struct {
	Follow     bool
	Timestamps bool
	Tail       string // "all" or a number string
}

// GetContainerLogs resolves the Deployment to its current Pod and streams logs.
// The returned ReadCloser must be closed by the caller.
func (a *KubernetesDockerAdapter) GetContainerLogs(ctx context.Context, name string, opts LogOptions) (io.ReadCloser, error) {
	resolved, err := a.resolveDeploymentName(ctx, name)
	if err != nil {
		return nil, err
	}

	pod, err := a.currentPodForDeployment(ctx, resolved)
	if err != nil {
		return nil, err
	}

	tailLines := int64(-1)
	if opts.Tail != "" && opts.Tail != "all" {
		n := int64(0)
		if _, scanErr := fmt.Sscan(opts.Tail, &n); scanErr == nil {
			tailLines = n
		}
	}

	logOpts := &corev1.PodLogOptions{
		Follow:     opts.Follow,
		Timestamps: opts.Timestamps,
	}
	if tailLines > 0 {
		logOpts.TailLines = &tailLines
	}

	req := a.client.CoreV1().Pods(a.namespace).GetLogs(pod.Name, logOpts)
	return req.Stream(ctx)
}

// currentPodForDeployment returns the first Running Pod owned by the named Deployment.
// Falls back to any pod if none is in Running phase (e.g. CrashLoopBackOff).
func (a *KubernetesDockerAdapter) currentPodForDeployment(ctx context.Context, deploymentName string) (*corev1.Pod, error) {
	pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s,%s=%s",
			deploymentName,
			types.LabelManagedBy,
			types.LabelManagedByValue,
		),
	})
	if err != nil {
		return nil, fmt.Errorf("unable to list pods for deployment %q: %w", deploymentName, err)
	}

	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			return &p, nil
		}
	}

	if len(pods.Items) > 0 {
		return &pods.Items[0], nil
	}

	return nil, fmt.Errorf("no pods found for deployment %q", deploymentName)
}
