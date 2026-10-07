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

// TestRunOncePodGoneWithoutExitCodeIsNotReportedAsCleanExit guards against a
// container whose pod was deleted by something other than d2k's own
// stopPod/removeContainer (a force-deleted pod, a node loss, an evicted
// pod): nothing ever wrote AnnotationStoppedAt or AnnotationExitCode, so the
// container must not be reported as a normal "Exited (0)" the way a real
// clean exit would be.
func TestRunOncePodGoneWithoutExitCodeIsNotReportedAsCleanExit(t *testing.T) {
	d := withReplicas(0, map[string]string{
		types.AnnotationRestartPolicy: "no",
		types.AnnotationStartedAt:     now.Add(-time.Minute).Format(time.RFC3339Nano),
	})
	got := podContainerState(d, nil, now)
	if got.Status != container.StateExited {
		t.Fatalf("status = %s, want %s", got.Status, container.StateExited)
	}
	if got.ExitCode == 0 {
		t.Fatalf("exit code = 0: a pod that vanished without d2k recording a stop looks like a clean exit, hiding that it was killed")
	}
	if got.FinishedAt == "" {
		t.Fatal("finished at is empty: no indication of when the container was lost")
	}
}

// A container whose pod Kubernetes replaces (kubectl delete pod, a drain,
// an eviction) is, as in Docker for a killed restart:always container,
// running until its process ends, then restarting until it runs again:
// never created. Attached compose up inspects it on the die and stops
// following a container that is neither (D2K-071).
func TestContainerStateWhilePodIsReplaced(t *testing.T) {
	started := map[string]string{types.AnnotationStartedAt: now.Add(-time.Hour).Format(time.RFC3339Nano)}
	finished := metav1.NewTime(now.Add(-time.Second))
	replaced := func(state corev1.ContainerState) corev1.Pod {
		p := podWith(corev1.ContainerStatus{State: state})
		p.CreationTimestamp = metav1.NewTime(now.Add(-time.Hour))
		p.DeletionTimestamp = &metav1.Time{Time: now.Add(-5 * time.Second)}
		return p
	}
	creating := podWith(corev1.ContainerStatus{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}})
	creating.CreationTimestamp = metav1.NewTime(now.Add(-5 * time.Second))
	scheduled := corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: creating.CreationTimestamp}}

	for _, tt := range []struct {
		name string
		pods []corev1.Pod
		want container.ContainerState
	}{
		{"old process still stopping", []corev1.Pod{replaced(corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Hour))}}), creating}, container.StateRunning},
		{"old process ended, new one creating", []corev1.Pod{replaced(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, FinishedAt: finished}}), creating}, container.StateRestarting},
		{"old process ended, new pod not started", []corev1.Pod{replaced(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, FinishedAt: finished}}), scheduled}, container.StateRestarting},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := containerState(withReplicas(1, started), tt.pods, now)
			if got.Status != tt.want || !got.Running {
				t.Fatalf("state = %s (running %v), want %s", got.Status, got.Running, tt.want)
			}
		})
	}

	// An evicted pod is kept, Failed, after its replacement ran: it is not
	// the run of a later docker start.
	evicted := podWith(corev1.ContainerStatus{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, FinishedAt: finished}}})
	evicted.CreationTimestamp, evicted.Status.Phase = metav1.NewTime(now.Add(-2*time.Hour)), corev1.PodFailed
	if got := containerState(withReplicas(1, started), []corev1.Pod{evicted, creating}, now); got.Status != container.StateCreated {
		t.Fatalf("starting beside an evicted pod: state = %s, want %s", got.Status, container.StateCreated)
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

// A crash-looping container's RestartCount must come from its pod, the way
// a real daemon's own restart manager counts it (D2K-080): docker inspect
// and the "restart count" Portainer reads from it must not stay 0 while a
// restart:always/on-failure container keeps crashing.
func TestContainerRestartCount(t *testing.T) {
	started := map[string]string{types.AnnotationStartedAt: now.Add(-time.Hour).Format(time.RFC3339Nano)}
	crashLooping := podWith(corev1.ContainerStatus{
		RestartCount: 5,
		State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	})
	if got := containerRestartCount(withReplicas(1, started), []corev1.Pod{crashLooping}); got != 5 {
		t.Errorf("restart count = %d, want 5 for a crash-looping Deployment-backed container", got)
	}

	onFailure := podWith(corev1.ContainerStatus{
		RestartCount: 2,
		State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	})
	if got := containerRestartCount(withReplicas(0, map[string]string{
		types.AnnotationRestartPolicy: "on-failure",
		types.AnnotationStartedAt:     now.Add(-time.Hour).Format(time.RFC3339Nano),
	}), []corev1.Pod{onFailure}); got != 2 {
		t.Errorf("restart count = %d, want 2 for a crash-looping run-once container", got)
	}

	if got := containerRestartCount(withReplicas(0, nil), nil); got != 0 {
		t.Errorf("restart count = %d, want 0 with no pod", got)
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
