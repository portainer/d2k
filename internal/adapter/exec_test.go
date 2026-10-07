package adapter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

// execClient is a fake cluster whose pod exec requests reach server.
type execClient struct {
	*fake.Clientset
	rest rest.Interface
}

func (c execClient) CoreV1() corev1client.CoreV1Interface {
	return execCoreV1{c.Clientset.CoreV1(), c.rest}
}

type execCoreV1 struct {
	corev1client.CoreV1Interface
	rest rest.Interface
}

func (c execCoreV1) RESTClient() rest.Interface { return c.rest }

// newExecAdapter returns an adapter whose exec and attach requests are
// recorded, by path, and refused.
func newExecAdapter(t *testing.T) (*KubernetesDockerAdapter, *fake.Clientset, func() []string) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		http.Error(w, "recorded", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	cfg := &rest.Config{Host: srv.URL, APIPath: "/api", ContentConfig: rest.ContentConfig{
		GroupVersion: &corev1.SchemeGroupVersion, NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
	}}
	rc, err := rest.RESTClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset()
	var k kubernetes.Interface = execClient{client, rc}
	a := NewWithClient(k, "d2k")
	a.restConfig = cfg
	return a, client, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// When Kubernetes replaces a container's pod (deleted by hand, drained,
// evicted), inspect reports the container running from the old pod until
// the new pod's container starts. An exec meanwhile must run in the new
// pod once it starts, not fail "not running" while inspect says running.
func TestExecDuringPodReplacementRunsInTheNewPod(t *testing.T) {
	a, client, posted := newExecAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := a.CreateContainer(ctx, RunOptions{Name: "web", Image: "nginx", RestartPolicy: string(container.RestartPolicyAlways)}); err != nil {
		t.Fatal(err)
	}
	if err := a.StartContainer(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"app": "web", "d2k.portainer.io/managed-by": "d2k"}
	started := metav1.NewTime(time.Now().Add(-time.Minute))
	deleting := metav1.Now()
	for _, p := range []*corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "web-old", Namespace: "d2k", Labels: labels, CreationTimestamp: started, DeletionTimestamp: &deleting},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}},
		}}},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: "web-new", Namespace: "d2k", Labels: labels, CreationTimestamp: metav1.Now()},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}}},
	}} {
		if _, err := client.CoreV1().Pods("d2k").Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if ctr, err := a.InspectContainer(ctx, "web"); err != nil || !ctr.State.Running {
		t.Fatalf("inspect during the replacement = %+v, %v; want running", ctr.State, err)
	}

	go func() {
		time.Sleep(300 * time.Millisecond)
		p, err := client.CoreV1().Pods("d2k").Get(ctx, "web-new", metav1.GetOptions{})
		if err != nil {
			return
		}
		p.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
		}}}
		_, _ = client.CoreV1().Pods("d2k").UpdateStatus(ctx, p, metav1.UpdateOptions{})
	}()

	_, err := a.ExecContainer(ctx, ExecOptions{Name: "web", Cmd: []string{"true"}, AttachStdout: true}, ExecStreams{Stdout: io.Discard})
	if err != nil && strings.Contains(err.Error(), "not running") {
		t.Fatalf("exec during the replacement = %v, want it run in the new pod", err)
	}
	if p := posted(); len(p) != 1 || !strings.HasSuffix(p[0], "/pods/web-new/exec") {
		t.Fatalf("exec requests = %v, want one to web-new", p)
	}
}
