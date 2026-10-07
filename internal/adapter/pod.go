package adapter

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/moby/moby/api/types/container"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/util/retry"

	"github.com/portainer/d2k/internal/types"
)

// Containers whose restart policy is "no" or "on-failure" run as a pod
// created from their Deployment's template on each start, so they can exit;
// the Deployment, never scaled up, is the container's record. The pod is
// controlled by the Deployment: its ReplicaSet leaves it alone, and removing
// the container deletes it.

// runsAsPod reports whether the container runs as a pod.
func runsAsPod(d appsv1.Deployment) bool {
	p := container.RestartPolicyMode(d.Annotations[types.AnnotationRestartPolicy])
	return p == container.RestartPolicyDisabled || p == container.RestartPolicyOnFailure
}

// restartPolicyOf returns the container's Docker restart policy. Containers
// created before it was recorded are always restarted.
func restartPolicyOf(d appsv1.Deployment) container.RestartPolicyMode {
	if p := d.Annotations[types.AnnotationRestartPolicy]; p != "" {
		return container.RestartPolicyMode(p)
	}
	return container.RestartPolicyAlways
}

// startPod runs a new pod for the container, replacing any previous one.
func (a *KubernetesDockerAdapter) startPod(ctx context.Context, d *appsv1.Deployment) error {
	pods := a.client.CoreV1().Pods(a.namespace)
	previous, err := pods.List(ctx, podsOf(d.Name))
	if err != nil {
		return fmt.Errorf("unable to list the pods of %q: %w", d.Name, err)
	}
	// As in Docker, a container that is running, or starting, keeps its run.
	if slices.ContainsFunc(previous.Items, func(p corev1.Pod) bool { return p.DeletionTimestamp == nil && !podEnded(p) }) {
		return nil
	}

	// The previous run is replaced only once the new pod exists: a start
	// Kubernetes rejects keeps it, and the container is never without a run.
	tmpl := d.Spec.Template.DeepCopy()
	tmpl.Spec.RestartPolicy = corev1.RestartPolicyNever
	if restartPolicyOf(*d) == container.RestartPolicyOnFailure {
		tmpl.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        truncate(d.Name, 57) + "-" + rand.String(5),
			Namespace:   a.namespace,
			Labels:      tmpl.Labels,
			Annotations: tmpl.Annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: d.Name, UID: d.UID, Controller: ptr(true),
			}},
		},
		Spec: tmpl.Spec,
	}
	if _, err := pods.Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("unable to create the pod of %q: %w", d.Name, err)
	}
	if err := a.annotate(ctx, d.Name, map[string]*string{
		types.AnnotationStartedAt: ptr(time.Now().UTC().Format(time.RFC3339Nano)),
		types.AnnotationStoppedAt: nil,
		types.AnnotationExitCode:  nil,
	}); err != nil {
		return err
	}
	for _, p := range previous.Items {
		if err := pods.Delete(ctx, p.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("unable to delete the previous pod of %q: %w", d.Name, err)
		}
	}
	return nil
}

// stopPod stops the container's pod, as docker stop does: it waits for the
// pod to end and records its exit code. A pod that has ended is kept, with
// its logs.
func (a *KubernetesDockerAdapter) stopPod(ctx context.Context, d *appsv1.Deployment) error {
	pods := a.client.CoreV1().Pods(a.namespace)
	list, err := pods.List(ctx, podsOf(d.Name))
	if err != nil {
		return fmt.Errorf("unable to list the pods of %q: %w", d.Name, err)
	}
	grace := int64(corev1.DefaultTerminationGracePeriodSeconds)
	if g := d.Spec.Template.Spec.TerminationGracePeriodSeconds; g != nil {
		grace = *g
	}
	exitCode := ""
	for _, p := range list.Items {
		if podEnded(p) {
			continue
		}
		if err := pods.Delete(ctx, p.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("unable to stop the pod of %q: %w", d.Name, err)
		}
		if code, ok := a.waitForPodGone(ctx, p.Name, time.Duration(grace+30)*time.Second); ok {
			exitCode = code
		}
	}
	updates := map[string]*string{types.AnnotationStoppedAt: ptr(time.Now().UTC().Format(time.RFC3339Nano))}
	if exitCode != "" {
		updates[types.AnnotationExitCode] = &exitCode
	}
	err = a.annotate(ctx, d.Name, updates)
	if apierrors.IsNotFound(err) && d.Annotations[types.AnnotationAutoRemove] == "true" {
		// The end of its run removed the container meanwhile.
		return nil
	}
	return err
}

// waitForPodGone waits until the pod is deleted, and returns its container's
// exit code if it saw one.
func (a *KubernetesDockerAdapter) waitForPodGone(ctx context.Context, name string, timeout time.Duration) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code, seen := "", false
	for {
		p, err := a.client.CoreV1().Pods(a.namespace).Get(ctx, name, metav1GetOptions())
		if err != nil {
			return code, seen
		}
		if s := p.Status.ContainerStatuses; len(s) > 0 && s[0].State.Terminated != nil {
			code, seen = strconv.Itoa(int(s[0].State.Terminated.ExitCode)), true
		}
		select {
		case <-ctx.Done():
			return code, seen
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// waitForPodsGone waits until the Deployment has no pods left, and returns
// the exit code of their container if it saw one.
func (a *KubernetesDockerAdapter) waitForPodsGone(ctx context.Context, deployment string, timeout time.Duration) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code, seen := "", false
	for {
		pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, podsOf(deployment))
		if err != nil || len(pods.Items) == 0 {
			return code, seen
		}
		for _, p := range pods.Items {
			if s := p.Status.ContainerStatuses; len(s) > 0 && s[0].State.Terminated != nil {
				code, seen = strconv.Itoa(int(s[0].State.Terminated.ExitCode)), true
			}
		}
		select {
		case <-ctx.Done():
			return code, seen
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// restartStartTimeout bounds how long a restart waits for the container to
// run again. A pod that takes longer, pulling its image on another node for
// one, is reported starting meanwhile.
const restartStartTimeout = 30 * time.Second

// waitUntilRunStarted waits until the container's run has begun, or failed
// to, or timeout passes.
func (a *KubernetesDockerAdapter) waitUntilRunStarted(ctx context.Context, deployment string, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		d, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, deployment, metav1GetOptions())
		if err != nil {
			return
		}
		pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, podsOf(deployment))
		if err != nil {
			return
		}
		if state := containerState(*d, pods.Items, time.Now()); !IsStarting(&state) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// podCreateTimeout bounds how long a start waits for Kubernetes to create
// the container's pod, or refuse to.
const podCreateTimeout = 10 * time.Second

// waitUntilPodCreated waits until the Deployment's pod has been created,
// and fails with the reason Kubernetes gives when it cannot be, as a full pod
// quota makes it (D2K-084): Docker's start fails when the container cannot
// run. A pod that waits to be scheduled is not a failure here.
func (a *KubernetesDockerAdapter) waitUntilPodCreated(ctx context.Context, deployment string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		d, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, deployment, metav1GetOptions())
		if err != nil {
			return nil
		}
		// Once it has seen the start, the Deployment controller copies its
		// ReplicaSet's failure to create the pod onto the Deployment. It
		// always records a condition: with none, no controller runs it.
		if d.Status.ObservedGeneration >= d.Generation {
			if d.Status.Replicas > 0 || len(d.Status.Conditions) == 0 {
				return nil
			}
			for _, c := range d.Status.Conditions {
				if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
					return fmt.Errorf("unable to start container %q: %s", dockerName(*d), c.Message)
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// podEnded reports whether the pod's container has exited for good. The
// kubelet reports the container terminated, which is the container's die,
// before it reports the pod's phase.
func podEnded(p corev1.Pod) bool {
	if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return true
	}
	s := p.Status.ContainerStatuses
	if len(s) == 0 || s[0].State.Terminated == nil {
		return false
	}
	return p.Spec.RestartPolicy == corev1.RestartPolicyNever || p.Spec.RestartPolicy == corev1.RestartPolicyOnFailure && s[0].State.Terminated.ExitCode == 0
}

// annotate sets (or, for nil values, removes) annotations of the Deployment.
func (a *KubernetesDockerAdapter) annotate(ctx context.Context, name string, updates map[string]*string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		d, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, name, metav1GetOptions())
		if err != nil {
			return err
		}
		if d.Annotations == nil {
			d.Annotations = map[string]string{}
		}
		for k, v := range updates {
			if v == nil {
				delete(d.Annotations, k)
			} else {
				d.Annotations[k] = *v
			}
		}
		_, err = a.client.AppsV1().Deployments(a.namespace).Update(ctx, d, metav1.UpdateOptions{})
		return err
	})
}

func ptr[T any](v T) *T { return &v }
