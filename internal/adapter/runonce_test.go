package adapter

import (
	"archive/tar"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/portainer/d2k/internal/errdefs"
)

func TestRestartPolicyTranslation(t *testing.T) {
	for _, tt := range []struct{ body, want, warning string }{
		{`{"Image":"app"}`, "no", ""},
		{`{"Image":"app","HostConfig":{"RestartPolicy":{"Name":"no"}}}`, "no", ""},
		{`{"Image":"app","HostConfig":{"RestartPolicy":{"Name":"on-failure","MaximumRetryCount":3}}}`, "on-failure", "maximum retry count 3 ignored"},
		{`{"Image":"app","HostConfig":{"RestartPolicy":{"Name":"always"}}}`, "always", ""},
		{`{"Image":"app","HostConfig":{"RestartPolicy":{"Name":"unless-stopped"}}}`, "unless-stopped", ""},
	} {
		opts, warnings := RunOptionsFromRequest("web", request(t, tt.body))
		if opts.RestartPolicy != tt.want {
			t.Errorf("%s: restart policy = %q, want %q", tt.body, opts.RestartPolicy, tt.want)
		}
		if text := strings.Join(warnings, "\n"); (tt.warning == "") != (text == "") || !strings.Contains(text, tt.warning) {
			t.Errorf("%s: warnings = %q, want %q", tt.body, warnings, tt.warning)
		}
	}
}

// podOf returns the container's pod, which must be the only one.
func podOf(t *testing.T, a *KubernetesDockerAdapter, name string) *corev1.Pod {
	t.Helper()
	pods, err := a.client.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app=" + name})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods of %s = %d, %v; want 1", name, len(pods.Items), err)
	}
	return &pods.Items[0]
}

func setStatus(t *testing.T, a *KubernetesDockerAdapter, p *corev1.Pod, phase corev1.PodPhase, state corev1.ContainerState) {
	t.Helper()
	p.Status = corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{{State: state}}}
	if _, err := a.client.CoreV1().Pods(testNamespace).UpdateStatus(context.Background(), p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A container with restart policy "no" (Docker's default) runs once, as a
// pod, so it exits and reports its exit code.
func TestRunOnceContainer(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	id, _, err := a.CreateContainer(ctx, RunOptions{Name: "job", Image: "busybox", RestartPolicy: "no"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	d, _ := client.AppsV1().Deployments(testNamespace).Get(ctx, "job", metav1.GetOptions{})
	if replicasOf(d) != 0 {
		t.Errorf("replicas = %d, want 0: the pod runs the container", replicasOf(d))
	}
	p := podOf(t, a, "job")
	if p.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("pod restart policy = %q, want Never", p.Spec.RestartPolicy)
	}
	if ref := metav1.GetControllerOf(p); ref == nil || ref.UID != d.UID {
		t.Errorf("pod controller = %+v, want the container's Deployment", ref)
	}

	state := func() *container.State {
		t.Helper()
		ctr, err := a.InspectContainer(ctx, "job")
		if err != nil {
			t.Fatal(err)
		}
		if ctr.HostConfig.RestartPolicy.Name != "no" {
			t.Errorf("inspected restart policy = %q, want no", ctr.HostConfig.RestartPolicy.Name)
		}
		return ctr.State
	}
	if s := state(); s.Status != container.StateCreated || s.StartedAt == "" {
		t.Errorf("state of a pending pod = %+v, want created and started", s)
	}
	setStatus(t, a, p, corev1.PodRunning, corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}})
	if s := state(); s.Status != container.StateRunning || !s.Running {
		t.Errorf("state of a running pod = %+v", s)
	}
	setStatus(t, a, p, corev1.PodFailed, corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3, FinishedAt: metav1.Now()}})
	if s := state(); s.Status != container.StateExited || s.Running || s.ExitCode != 3 {
		t.Errorf("state of an exited pod = %+v, want exited with code 3", s)
	}

	// Starting again runs a new pod.
	if err := a.StartContainer(ctx, "job"); err != nil {
		t.Fatal(err)
	}
	if again := podOf(t, a, "job"); again.Name == p.Name {
		t.Error("start reused the exited pod")
	}
	if err := a.StopContainer(ctx, "job"); err != nil {
		t.Fatal(err)
	}
	pods, _ := client.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app=job"})
	if len(pods.Items) != 0 {
		t.Errorf("pods after stop = %d, want 0", len(pods.Items))
	}
	if s := state(); s.Status != container.StateExited {
		t.Errorf("state after stop = %+v, want exited", s)
	}

	// on-failure restarts the pod's container on failure only.
	if _, _, err := a.CreateContainer(ctx, RunOptions{Name: "retry", Image: "busybox", RestartPolicy: "on-failure"}); err != nil {
		t.Fatal(err)
	}
	if err := a.StartContainer(ctx, "retry"); err != nil {
		t.Fatal(err)
	}
	if p := podOf(t, a, "retry"); p.Spec.RestartPolicy != corev1.RestartPolicyOnFailure {
		t.Errorf("on-failure pod restart policy = %q", p.Spec.RestartPolicy)
	}

	// Files cannot be copied into the running pod.
	setStatus(t, a, podOf(t, a, "retry"), corev1.PodRunning, corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}})
	if err := a.CopyToContainer(ctx, "retry", "/", archive(t, file{name: "/x", content: "x", kind: tar.TypeReg})); !errdefs.IsNotImplemented(err) {
		t.Errorf("copy into a running pod = %v, want not implemented", err)
	}
}

func TestRunOnceContainerDies(t *testing.T) {
	d := deployment("job", "uid-1", managed())
	known := map[string]*appsv1.Deployment{"job": d}
	restarts, terminations := map[string]int32{}, map[string]int32{}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "job-x", Labels: map[string]string{"app": "job"}},
		Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever}}
	if msgs := podEvents(known, restarts, terminations, watch.Added, p, time.Now()); len(msgs) != 0 {
		t.Fatalf("events for a new pod = %+v", msgs)
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}}}}
	msgs := podEvents(known, restarts, terminations, watch.Modified, p, time.Now())
	if len(msgs) != 1 || msgs[0].Action != events.ActionDie || msgs[0].Actor.Attributes["exitCode"] != "3" {
		t.Fatalf("events for an exited pod = %+v, want die with exit code 3", msgs)
	}
	if msgs := podEvents(known, restarts, terminations, watch.Modified, p, time.Now()); len(msgs) != 0 {
		t.Fatalf("events for the same exit = %+v, want none", msgs)
	}
}

// As in Docker, a started container with a healthcheck reports "starting"
// until the check has a result, even before its pod runs.
func TestRunOnceContainerHealthWhileStarting(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()
	probe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}}
	if _, _, err := a.CreateContainer(ctx, RunOptions{Name: "db", Image: "redis", RestartPolicy: "no", Healthcheck: probe}); err != nil {
		t.Fatal(err)
	}
	if err := a.StartContainer(ctx, "db"); err != nil {
		t.Fatal(err)
	}
	ctr, err := a.InspectContainer(ctx, "db")
	if err != nil {
		t.Fatal(err)
	}
	if ctr.State.Health == nil || ctr.State.Health.Status != container.Starting {
		t.Fatalf("health of a starting container = %+v, want starting", ctr.State.Health)
	}
}

// Pods that share the container's app label but are not d2k's, such as a
// StatefulSet's in the same namespace, are left alone.
func TestRunOnceLeavesOtherPodsAlone(t *testing.T) {
	other := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: testNamespace, Labels: map[string]string{"app": "web"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}},
		}},
	}
	a, client := newTestAdapter(t, other)
	ctx := context.Background()
	mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", RestartPolicy: "no"})

	if err := a.StartContainer(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Pods(testNamespace).Get(ctx, "web-0", metav1.GetOptions{}); err != nil {
		t.Fatalf("start deleted a pod d2k does not own: %v", err)
	}
	if ctr, err := a.InspectContainer(ctx, "web"); err != nil || ctr.State.Running {
		t.Fatalf("inspect = %+v, %v; want the state of d2k's pod, not running", ctr.State, err)
	}
	if err := a.StopContainer(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Pods(testNamespace).Get(ctx, "web-0", metav1.GetOptions{}); err != nil {
		t.Fatalf("stop deleted a pod d2k does not own: %v", err)
	}
}

// As in Docker, starting a container that is running leaves it running:
// its process is not restarted.
func TestStartRunningContainerKeepsItsRun(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()
	mustCreate(t, a, RunOptions{Name: "job", Image: "busybox", RestartPolicy: "no"})
	if err := a.StartContainer(ctx, "job"); err != nil {
		t.Fatal(err)
	}
	first := podOf(t, a, "job")
	if err := a.StartContainer(ctx, "job"); err != nil {
		t.Fatalf("start of a starting container = %v", err)
	}
	setStatus(t, a, podOf(t, a, "job"), corev1.PodRunning, corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}})
	if err := a.StartContainer(ctx, "job"); err != nil {
		t.Fatalf("start of a running container = %v", err)
	}
	if p := podOf(t, a, "job"); p.Name != first.Name {
		t.Fatalf("pod after starting a running container = %s, want %s: the run was replaced", p.Name, first.Name)
	}
}

// A run whose pod disappears is reported killed (D2K-026), even when an
// earlier run of the container was stopped by docker stop.
func TestRunOncePodGoneAfterStopAndStart(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	mustCreate(t, a, RunOptions{Name: "job", Image: "busybox", RestartPolicy: "no"})
	if err := a.StartContainer(ctx, "job"); err != nil {
		t.Fatal(err)
	}
	setStatus(t, a, podOf(t, a, "job"), corev1.PodRunning, corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}})
	if err := a.StopContainer(ctx, "job"); err != nil {
		t.Fatal(err)
	}
	if err := a.StartContainer(ctx, "job"); err != nil {
		t.Fatal(err)
	}
	if err := client.CoreV1().Pods(testNamespace).Delete(ctx, podOf(t, a, "job").Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	ctr, err := a.InspectContainer(ctx, "job")
	if err != nil {
		t.Fatal(err)
	}
	if s := ctr.State; s.Status != container.StateExited || s.ExitCode != 137 {
		t.Fatalf("state after the pod of the second run is lost = %s, exit %d, finished %s; want exited 137, not the first run's stop", s.Status, s.ExitCode, s.FinishedAt)
	}
}
