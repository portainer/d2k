package adapter

import (
	"context"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/internal/types"
)

// startWith starts the container while the kubelet reports state on its
// pod (run-once) or on a pod it adds for its Deployment, and returns the
// start's error.
func startWith(t *testing.T, a *KubernetesDockerAdapter, name string, runOnce bool, state corev1.ContainerState) error {
	t.Helper()
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- a.StartContainer(ctx, name) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		pods, _ := a.client.CoreV1().Pods(testNamespace).List(ctx, podsOf(name))
		if !runOnce && len(pods.Items) == 0 {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-abc", Namespace: testNamespace,
				Labels: map[string]string{"app": name, types.LabelManagedBy: types.LabelManagedByValue}}}
			if _, err := a.client.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if len(pods.Items) > 0 {
			p := pods.Items[0]
			p.Status = corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{State: state}}}
			if _, err := a.client.CoreV1().Pods(testNamespace).UpdateStatus(ctx, &p, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no pod to report on")
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("start did not return")
		return nil
	}
}

func waiting(reason, message string) corev1.ContainerState {
	return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}}
}

// As Docker's docker run of an image that doesn't exist fails ("pull
// access denied ... repository does not exist"), so does d2k's start: the
// kubelet would retry the pull forever, and docker run, docker wait and
// an attached compose up hung (D2K-068). The container is left stopped.
func TestStartFailsWhenTheImageCannotBePulled(t *testing.T) {
	notFound := `failed to pull and unpack image "docker.io/library/nosuchimage:latest": failed to resolve reference "docker.io/library/nosuchimage:latest": pull access denied, repository does not exist or may require authorization: server message: insufficient_scope: authorization failed`
	for _, tc := range []struct {
		name, reason, message string
		runOnce               bool
		kind                  func(error) bool
	}{
		{"run-once, image not found", "ErrImagePull", notFound, true, errdefs.IsNotFound},
		{"restart always, image not found", "ErrImagePull", notFound, false, errdefs.IsNotFound},
		{"tag not found, in back-off", "ImagePullBackOff", `Back-off pulling image "nginx:nosuchtag": ErrImagePull: failed to pull and unpack image "docker.io/library/nginx:nosuchtag": failed to resolve reference "docker.io/library/nginx:nosuchtag": docker.io/library/nginx:nosuchtag: not found`, true, errdefs.IsNotFound},
		{"private image, no access", "ErrImagePull", `failed to pull and unpack image "registry.example.com/team/app:1": failed to authorize: failed to fetch anonymous token: unexpected status: 401 Unauthorized`, true, errdefs.IsNotFound},
		{"invalid image name", "InvalidImageName", `Failed to apply default image tag "Bad Name": couldn't parse image name "Bad Name": invalid reference format`, true, errdefs.IsInvalidParameter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, client := newTestAdapter(t)
			a.startWait = 5 * time.Second
			policy := "always"
			if tc.runOnce {
				policy = "no"
			}
			mustCreate(t, a, RunOptions{Name: "app", Image: "nosuchimage", RestartPolicy: policy})
			err := startWith(t, a, "app", tc.runOnce, waiting(tc.reason, tc.message))
			if err == nil || !tc.kind(err) {
				t.Fatalf("start = %v, want a %s error", err, tc.name)
			}
			if ctr, _ := a.InspectContainer(context.Background(), "app"); ctr == nil || ctr.State.Status == container.StateRunning || ctr.State.Running {
				t.Errorf("container after the failed start = %+v, want not running", ctr)
			}
			if !tc.runOnce {
				d, _ := client.AppsV1().Deployments(testNamespace).Get(context.Background(), "app", metav1.GetOptions{})
				if replicasOf(d) != 0 {
					t.Errorf("replicas = %d, want 0: it would keep pulling", replicasOf(d))
				}
			} else if pods, _ := client.CoreV1().Pods(testNamespace).List(context.Background(), podsOf("app")); len(pods.Items) != 0 {
				t.Errorf("pods = %d, want none left retrying the pull", len(pods.Items))
			}
		})
	}
}

// A pull error the kubelet may get past (the registry unreachable or rate
// limiting) is not a failed start: the kubelet retries, as Docker's pull
// would have waited. Nor is a container that starts.
func TestStartWaitsOutRetryablePullErrors(t *testing.T) {
	for _, state := range []corev1.ContainerState{
		waiting("ErrImagePull", `failed to pull and unpack image "docker.io/library/nginx:latest": failed to do request: Head "https://registry-1.docker.io/v2/library/nginx/manifests/latest": dial tcp: i/o timeout`),
		waiting("ErrImagePull", `failed to pull and unpack image "docker.io/library/nginx:latest": unexpected status: 429 Too Many Requests`),
		{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
	} {
		a, _ := newTestAdapter(t)
		a.startWait = time.Second
		mustCreate(t, a, RunOptions{Name: "web", Image: "nginx", RestartPolicy: "no"})
		if err := startWith(t, a, "web", true, state); err != nil {
			t.Errorf("start with %+v = %v, want it started", state, err)
		}
	}
}
