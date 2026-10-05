package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/internal/types"
)

// LogOptions mirrors the docker logs settings Kubernetes can apply.
type LogOptions struct {
	Follow     bool
	Timestamps bool
	Tail       string // "all" (or empty) or a number of lines
	Since      time.Time
}

// errNoPod means the container's Deployment has no pod: it never ran.
var errNoPod = errors.New("no pod")

// GetContainerLogs streams the logs of the container's current pod, and
// reports whether the container runs with a TTY. A container that never ran
// has an empty log. The caller closes the stream.
func (a *KubernetesDockerAdapter) GetContainerLogs(ctx context.Context, name string, opts LogOptions) (io.ReadCloser, bool, error) {
	resolved, err := a.resolveDeploymentName(ctx, name)
	if err != nil {
		return nil, false, err
	}
	d, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, resolved, metav1.GetOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("unable to get deployment %q: %w", resolved, err)
	}
	tty := len(d.Spec.Template.Spec.Containers) > 0 && d.Spec.Template.Spec.Containers[0].TTY

	logOpts := &corev1.PodLogOptions{Follow: opts.Follow, Timestamps: opts.Timestamps}
	if opts.Tail != "" && opts.Tail != "all" {
		n, err := strconv.ParseInt(opts.Tail, 10, 64)
		if err != nil || n < 0 {
			return nil, false, errdefs.InvalidParameter(fmt.Errorf("invalid tail %q: expected \"all\" or a number of lines", opts.Tail))
		}
		logOpts.TailLines = &n
	}
	if !opts.Since.IsZero() {
		since := metav1.NewTime(opts.Since)
		logOpts.SinceTime = &since
	}

	pod, err := a.currentPodForDeployment(ctx, resolved)
	if errors.Is(err, errNoPod) {
		return io.NopCloser(strings.NewReader("")), tty, nil
	}
	if err != nil {
		return nil, false, err
	}
	stream, err := a.client.CoreV1().Pods(a.namespace).GetLogs(pod.Name, logOpts).Stream(ctx)
	return stream, tty, err
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

	// Pods being deleted belong to runs that are over.
	current := slices.DeleteFunc(pods.Items, func(p corev1.Pod) bool { return p.DeletionTimestamp != nil })
	for _, p := range current {
		if p.Status.Phase == corev1.PodRunning {
			return &p, nil
		}
	}

	if len(current) > 0 {
		newest := slices.MaxFunc(current, func(a, b corev1.Pod) int { return a.CreationTimestamp.Compare(b.CreationTimestamp.Time) })
		return &newest, nil
	}

	return nil, fmt.Errorf("deployment %q: %w", deploymentName, errNoPod)
}
