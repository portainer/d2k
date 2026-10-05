package adapter

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/moby/moby/api/types/container"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/portainer/d2k/internal/types"
)

// waitingWhileCreating are the reasons a starting container waits that are
// part of normal startup rather than an error to report.
var waitingWhileCreating = map[string]bool{"ContainerCreating": true, "PodInitializing": true}

// containerState derives the Docker state of the container a Deployment
// backs from the Deployment and its pods.
func containerState(d appsv1.Deployment, pods []corev1.Pod, now time.Time) container.State {
	if runsAsPod(d) {
		return podContainerState(d, pods, now)
	}
	state := container.State{Status: container.StateCreated, StartedAt: d.Annotations[types.AnnotationStartedAt]}
	if d.Spec.Replicas == nil || *d.Spec.Replicas == 0 {
		if state.StartedAt != "" {
			state.Status = container.StateExited
			state.FinishedAt = d.Annotations[types.AnnotationStoppedAt]
		}
		return state
	}

	status := newestContainerStatus(pods)
	switch {
	case status == nil:
	case status.State.Running != nil:
		state.Status, state.Running = container.StateRunning, true
		state.StartedAt = status.State.Running.StartedAt.UTC().Format(time.RFC3339Nano)
		state.Health = containerHealth(d, status, now)
	case status.RestartCount > 0 || status.State.Terminated != nil:
		// The Deployment restarts the container, as Docker's restart policy would.
		state.Status, state.Running, state.Restarting = container.StateRestarting, true, true
		if t := lastTermination(status); t != nil {
			state.ExitCode = int(t.ExitCode)
			state.FinishedAt = t.FinishedAt.UTC().Format(time.RFC3339Nano)
		}
	case status.State.Waiting != nil && !waitingWhileCreating[status.State.Waiting.Reason]:
		state.Error = status.State.Waiting.Reason + ": " + status.State.Waiting.Message
	}
	// As in Docker, a started container with a healthcheck is "starting"
	// until the check has a result, whatever the pod is doing meanwhile.
	if state.Health == nil && healthProbeOf(d) != nil {
		state.Health = &container.Health{Status: container.Starting}
	}
	return state
}

// podContainerState derives the state of a container that runs as a pod.
func podContainerState(d appsv1.Deployment, pods []corev1.Pod, now time.Time) container.State {
	state := container.State{Status: container.StateCreated, StartedAt: d.Annotations[types.AnnotationStartedAt]}
	if state.StartedAt == "" {
		return state
	}
	status := newestContainerStatus(pods)
	switch {
	case status == nil:
		// Without a pod, the container was stopped; with one, it is starting.
		if !slices.ContainsFunc(pods, func(p corev1.Pod) bool { return p.DeletionTimestamp == nil }) {
			state.Status, state.FinishedAt = container.StateExited, d.Annotations[types.AnnotationStoppedAt]
			state.ExitCode, _ = strconv.Atoi(d.Annotations[types.AnnotationExitCode])
		}
	case status.State.Running != nil:
		state.Status, state.Running = container.StateRunning, true
		state.StartedAt = status.State.Running.StartedAt.UTC().Format(time.RFC3339Nano)
		state.Health = containerHealth(d, status, now)
	case exitedForGood(d, status.State.Terminated):
		t := status.State.Terminated
		state.Status, state.ExitCode = container.StateExited, int(t.ExitCode)
		state.StartedAt = t.StartedAt.UTC().Format(time.RFC3339Nano)
		state.FinishedAt = t.FinishedAt.UTC().Format(time.RFC3339Nano)
	case status.State.Terminated != nil || status.RestartCount > 0:
		// on-failure: the kubelet restarts the failed container.
		state.Status, state.Running, state.Restarting = container.StateRestarting, true, true
		if t := lastTermination(status); t != nil {
			state.ExitCode = int(t.ExitCode)
			state.FinishedAt = t.FinishedAt.UTC().Format(time.RFC3339Nano)
		}
	case status.State.Waiting != nil && !waitingWhileCreating[status.State.Waiting.Reason]:
		state.Error = status.State.Waiting.Reason + ": " + status.State.Waiting.Message
	}
	if state.Status != container.StateExited && state.Health == nil && healthProbeOf(d) != nil {
		state.Health = &container.Health{Status: container.Starting}
	}
	return state
}

// exitedForGood reports whether a terminated container of a pod will not be
// restarted: always with restart policy "no", on success with "on-failure".
func exitedForGood(d appsv1.Deployment, t *corev1.ContainerStateTerminated) bool {
	return t != nil && (t.ExitCode == 0 || restartPolicyOf(d) != container.RestartPolicyOnFailure)
}

// IsStarting reports whether a container was started and its pod is not
// running yet, nor failing. Docker's start returns once the container runs,
// so clients treat such a container as running.
func IsStarting(s *container.State) bool {
	return s != nil && s.Status == container.StateCreated && s.StartedAt != "" && s.Error == ""
}

// healthProbeOf returns the readiness probe carrying the container's
// healthcheck, if it has one.
func healthProbeOf(d appsv1.Deployment) *corev1.Probe {
	if containers := d.Spec.Template.Spec.Containers; len(containers) > 0 {
		return containers[0].ReadinessProbe
	}
	return nil
}

// healthConfig reports the healthcheck a readiness probe was translated from.
func healthConfig(p *corev1.Probe) *container.HealthConfig {
	if p == nil || p.Exec == nil {
		return nil
	}
	test := append([]string{"CMD"}, p.Exec.Command...)
	if c := p.Exec.Command; len(c) == 3 && c[0] == "/bin/sh" && c[1] == "-c" {
		test = []string{"CMD-SHELL", c[2]}
	}
	return &container.HealthConfig{
		Test:        test,
		Interval:    time.Duration(p.PeriodSeconds) * time.Second,
		Timeout:     time.Duration(p.TimeoutSeconds) * time.Second,
		Retries:     int(p.FailureThreshold),
		StartPeriod: time.Duration(p.InitialDelaySeconds) * time.Second,
	}
}

// containerHealth derives Docker's health from the readiness probe that
// carries the container's healthcheck. Kubernetes reports only readiness,
// so a container not yet ready is "starting" while the probe could still
// succeed (start period plus retries), then "unhealthy".
func containerHealth(d appsv1.Deployment, status *corev1.ContainerStatus, now time.Time) *container.Health {
	p := healthProbeOf(d)
	if p == nil {
		return nil
	}
	if status.Ready {
		return &container.Health{Status: container.Healthy}
	}
	window := time.Duration(p.InitialDelaySeconds+p.PeriodSeconds*p.FailureThreshold) * time.Second
	if now.Sub(status.State.Running.StartedAt.Time) < window {
		return &container.Health{Status: container.Starting}
	}
	return &container.Health{Status: container.Unhealthy, FailingStreak: int(p.FailureThreshold)}
}

// newestContainerStatus returns the status of the container in the most
// recently created pod.
func newestContainerStatus(pods []corev1.Pod) *corev1.ContainerStatus {
	var newest *corev1.Pod
	for i := range pods {
		p := &pods[i]
		// A pod being deleted belongs to a run that is over.
		if p.DeletionTimestamp == nil && len(p.Status.ContainerStatuses) > 0 && (newest == nil || p.CreationTimestamp.After(newest.CreationTimestamp.Time)) {
			newest = p
		}
	}
	if newest == nil {
		return nil
	}
	return &newest.Status.ContainerStatuses[0]
}

func lastTermination(s *corev1.ContainerStatus) *corev1.ContainerStateTerminated {
	if s.State.Terminated != nil {
		return s.State.Terminated
	}
	return s.LastTerminationState.Terminated
}

// containerStatus formats a state the way docker ps does: "Up 5 minutes",
// "Exited (0) 2 minutes ago", "Restarting (1) 30 seconds ago", "Created".
func containerStatus(s container.State, now time.Time) string {
	since := func(ts string) string {
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return ""
		}
		return " " + humanizeDuration(now.Sub(t)) + " ago"
	}
	switch s.Status {
	case container.StateRunning:
		up := "Up"
		if t, err := time.Parse(time.RFC3339Nano, s.StartedAt); err == nil {
			up += " " + humanizeDuration(now.Sub(t))
		}
		if s.Health == nil {
			return up
		}
		if s.Health.Status == container.Starting {
			return up + " (health: starting)"
		}
		return up + " (" + string(s.Health.Status) + ")"
	case container.StateRestarting:
		return fmt.Sprintf("Restarting (%d)%s", s.ExitCode, since(s.FinishedAt))
	case container.StateExited:
		return fmt.Sprintf("Exited (%d)%s", s.ExitCode, since(s.FinishedAt))
	default:
		return "Created"
	}
}
