package adapter

import (
	"fmt"
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
func containerState(d appsv1.Deployment, pods []corev1.Pod) container.State {
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
	return state
}

// newestContainerStatus returns the status of the container in the most
// recently created pod.
func newestContainerStatus(pods []corev1.Pod) *corev1.ContainerStatus {
	var newest *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if len(p.Status.ContainerStatuses) > 0 && (newest == nil || p.CreationTimestamp.After(newest.CreationTimestamp.Time)) {
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
		if t, err := time.Parse(time.RFC3339Nano, s.StartedAt); err == nil {
			return "Up " + humanizeDuration(now.Sub(t))
		}
		return "Up"
	case container.StateRestarting:
		return fmt.Sprintf("Restarting (%d)%s", s.ExitCode, since(s.FinishedAt))
	case container.StateExited:
		return fmt.Sprintf("Exited (%d)%s", s.ExitCode, since(s.FinishedAt))
	default:
		return "Created"
	}
}
