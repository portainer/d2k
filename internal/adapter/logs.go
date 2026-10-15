package adapter

import (
	"bytes"
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
		// A stopped container's log went with its pod: serve the part kept
		// when it stopped, if any. A container that never ran has none.
		return io.NopCloser(strings.NewReader(keptLog(d.Annotations[types.AnnotationLastLog], logOpts))), tty, nil
	}
	if err != nil {
		return nil, false, err
	}
	pod = a.waitForContainerStarted(ctx, pod, 30*time.Second)
	stream, err := a.client.CoreV1().Pods(a.namespace).GetLogs(pod.Name, logOpts).Stream(ctx)
	return stream, tty, err
}

// waitForContainerStarted waits for pod's container to leave the transitional
// states (no container status yet, ContainerCreating, PodInitializing) that
// Docker has no equivalent of. The kubelet refuses to stream a container's
// logs during them, unlike Docker, which has nothing to refuse: a created
// container only starts running. A pod the scheduler cannot place is not
// waited for: it gets no container status. Left waiting past timeout, the pod is
// returned as is, so the kubelet's error passes through as before.
func (a *KubernetesDockerAdapter) waitForContainerStarted(ctx context.Context, pod *corev1.Pod, timeout time.Duration) *corev1.Pod {
	waiting := func(p *corev1.Pod) bool {
		s := p.Status.ContainerStatuses
		if len(s) == 0 {
			return !slices.ContainsFunc(p.Status.Conditions, func(c corev1.PodCondition) bool {
				return c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable
			})
		}
		return s[0].State.Waiting != nil && waitingWhileCreating[s[0].State.Waiting.Reason]
	}
	if !waiting(pod) {
		return pod
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for waiting(pod) {
		select {
		case <-ctx.Done():
			return pod
		case <-time.After(200 * time.Millisecond):
		}
		p, err := a.client.CoreV1().Pods(a.namespace).Get(ctx, pod.Name, metav1GetOptions())
		if err != nil {
			return pod
		}
		pod = p
	}
	return pod
}

// currentPodForDeployment returns the first Running Pod owned by the named Deployment.
// Falls back to any pod if none is in Running phase (e.g. CrashLoopBackOff).
func (a *KubernetesDockerAdapter) currentPodForDeployment(ctx context.Context, deploymentName string) (*corev1.Pod, error) {
	pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, podsOf(deploymentName))
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

// The part of a stopped container's log d2k keeps (AnnotationLastLog): the
// last keptLogLines lines, at most keptLogBytes, so the Deployment's
// annotations stay well below Kubernetes' 256 KiB limit.
const (
	keptLogLines = 1000
	keptLogBytes = 64 << 10
)

// logTail reads the last part of the pod's log, with timestamps, to keep
// when its container stops.
func (a *KubernetesDockerAdapter) logTail(ctx context.Context, pod string) (string, bool) {
	lines := int64(keptLogLines)
	stream, err := a.client.CoreV1().Pods(a.namespace).GetLogs(pod, &corev1.PodLogOptions{Timestamps: true, TailLines: &lines}).Stream(ctx)
	if err != nil {
		return "", false
	}
	defer func() { _ = stream.Close() }()
	// Bound the read: a thousand very long lines is still only kept in part.
	data, err := io.ReadAll(io.LimitReader(stream, 16*keptLogBytes))
	if err != nil {
		return "", false
	}
	if len(data) > keptLogBytes {
		data = data[len(data)-keptLogBytes:]
		// Keep whole lines only.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	return string(data), true
}

// keptLog applies docker logs' --since, --tail and --timestamps to a kept
// log, whose lines start with the kubelet's RFC 3339 timestamp.
func keptLog(log string, opts *corev1.PodLogOptions) string {
	var out []string
	for line := range strings.Lines(log) {
		stamp, text, _ := strings.Cut(line, " ")
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			// Not a timestamped line: keep it as it is.
			out = append(out, line)
			continue
		}
		if opts.SinceTime != nil && at.Before(opts.SinceTime.Time) {
			continue
		}
		if opts.Timestamps {
			text = line
		}
		out = append(out, text)
	}
	if opts.TailLines != nil && int64(len(out)) > *opts.TailLines {
		out = out[int64(len(out))-*opts.TailLines:]
	}
	return strings.Join(out, "")
}
