package adapter

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/portainer/d2k/internal/types"
)

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func withReplicas(n int32, annotations map[string]string) appsv1.Deployment {
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Annotations: annotations},
		Spec:       appsv1.DeploymentSpec{Replicas: &n},
	}
}

func podWith(status corev1.ContainerStatus) corev1.Pod {
	return corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{status}}}
}

func TestContainerState(t *testing.T) {
	started := map[string]string{
		types.AnnotationStartedAt: now.Add(-time.Hour).Format(time.RFC3339Nano),
		types.AnnotationStoppedAt: now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
	}
	runningSince := metav1.NewTime(now.Add(-5 * time.Minute))
	finished := metav1.NewTime(now.Add(-30 * time.Second))

	tests := []struct {
		name       string
		d          appsv1.Deployment
		pods       []corev1.Pod
		want       container.ContainerState
		exitCode   int
		statusText string
	}{
		{name: "created, never started", d: withReplicas(0, nil), want: container.StateCreated, statusText: "Created"},
		{name: "starting, no pod yet", d: withReplicas(1, started), want: container.StateCreated, statusText: "Created"},
		{name: "pulling the image", d: withReplicas(1, started),
			pods: []corev1.Pod{podWith(corev1.ContainerStatus{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}})},
			want: container.StateCreated, statusText: "Created"},
		{name: "running", d: withReplicas(1, started),
			pods: []corev1.Pod{podWith(corev1.ContainerStatus{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: runningSince}}})},
			want: container.StateRunning, statusText: "Up 5 minutes"},
		{name: "crash looping", d: withReplicas(1, started),
			pods: []corev1.Pod{podWith(corev1.ContainerStatus{
				RestartCount:         3,
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, FinishedAt: finished}},
			})},
			want: container.StateRestarting, exitCode: 1, statusText: "Restarting (1) 30 seconds ago"},
		{name: "stopped after running", d: withReplicas(0, started), want: container.StateExited, statusText: "Exited (0) 2 minutes ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containerState(tt.d, tt.pods, now)
			if got.Status != tt.want || got.ExitCode != tt.exitCode {
				t.Fatalf("state = %s (exit %d), want %s (exit %d)", got.Status, got.ExitCode, tt.want, tt.exitCode)
			}
			if got.Running != (tt.want == container.StateRunning || tt.want == container.StateRestarting) || got.Restarting != (tt.want == container.StateRestarting) {
				t.Errorf("Running = %v, Restarting = %v for %s", got.Running, got.Restarting, tt.want)
			}
			if s := containerStatus(got, now); s != tt.statusText {
				t.Errorf("status = %q, want %q", s, tt.statusText)
			}
		})
	}
}

func TestContainerStateImagePullErrorIsReported(t *testing.T) {
	got := containerState(withReplicas(1, nil), []corev1.Pod{podWith(corev1.ContainerStatus{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
		Reason: "ImagePullBackOff", Message: `Back-off pulling image "nope:latest"`,
	}}})}, now)
	if got.Status != container.StateCreated || !strings.Contains(got.Error, "nope:latest") {
		t.Fatalf("state = %s, error %q; want created with the pull error", got.Status, got.Error)
	}
}

func TestContainerHealth(t *testing.T) {
	probe := &corev1.Probe{PeriodSeconds: 5, FailureThreshold: 3, InitialDelaySeconds: 10}
	withProbe := withReplicas(1, nil)
	withProbe.Spec.Template.Spec.Containers = []corev1.Container{{ReadinessProbe: probe}}
	running := func(since time.Duration, ready bool) []corev1.Pod {
		return []corev1.Pod{podWith(corev1.ContainerStatus{Ready: ready, State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-since))}}})}
	}

	for _, tt := range []struct {
		name   string
		d      appsv1.Deployment
		pods   []corev1.Pod
		want   container.HealthStatus
		suffix string
	}{
		{"healthy", withProbe, running(time.Minute, true), container.Healthy, " (healthy)"},
		{"starting within the probe window", withProbe, running(20*time.Second, false), container.Starting, " (health: starting)"},
		{"unhealthy after the probe window", withProbe, running(time.Minute, false), container.Unhealthy, " (unhealthy)"},
		{"no healthcheck", withReplicas(1, nil), running(time.Minute, true), "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := containerState(tt.d, tt.pods, now)
			var got container.HealthStatus
			if s.Health != nil {
				got = s.Health.Status
			}
			if got != tt.want {
				t.Fatalf("health = %q, want %q", got, tt.want)
			}
			if status := containerStatus(s, now); !strings.HasSuffix(status, tt.suffix) || (tt.suffix == "" && strings.Contains(status, "(")) {
				t.Errorf("status = %q, want suffix %q", status, tt.suffix)
			}
		})
	}
}

func TestStartedContainerWithHealthcheckIsStarting(t *testing.T) {
	d := withReplicas(1, nil)
	d.Spec.Template.Spec.Containers = []corev1.Container{{ReadinessProbe: &corev1.Probe{PeriodSeconds: 5, FailureThreshold: 3}}}
	// The pod is not scheduled yet: compose still needs a health status.
	if s := containerState(d, nil, now); s.Health == nil || s.Health.Status != container.Starting {
		t.Fatalf("health = %+v, want starting", s.Health)
	}
	// Created, not started: no health, as in Docker.
	d = withReplicas(0, nil)
	d.Spec.Template.Spec.Containers = []corev1.Container{{ReadinessProbe: &corev1.Probe{}}}
	if s := containerState(d, nil, now); s.Health != nil {
		t.Fatalf("health of a created container = %+v, want none", s.Health)
	}
}

func TestHealthConfigRoundTrip(t *testing.T) {
	for _, test := range [][]string{{"CMD", "redis-cli", "ping"}, {"CMD-SHELL", "curl -f localhost || exit 1"}} {
		probe, _ := healthProbe(&container.HealthConfig{Test: test, Interval: 5 * time.Second, Retries: 4})
		got := healthConfig(probe)
		if !slices.Equal(got.Test, test) || got.Interval != 5*time.Second || got.Retries != 4 {
			t.Errorf("healthConfig(healthProbe(%q)) = %+v", test, got)
		}
	}
}
