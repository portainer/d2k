package router

import (
	"encoding/json"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/portainer/d2k/internal/adapter"
)

func node(name, cpu, memory string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		}},
	}
}

func info(t *testing.T, objs ...runtime.Object) map[string]any {
	t.Helper()
	client := fake.NewClientset(objs...)
	h := New(adapter.NewWithClient(client, "d2k"), "d2k", false)
	rec := do(t, h, "GET", "/info", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("info = %d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestInfoDescribesTheNamespace(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "d2k", UID: "6f1c2a7e-0000-4000-8000-000000000001"}}
	nodes := []runtime.Object{ns, node("a", "4", "8Gi"), node("b", "2", "4Gi")}

	body := info(t, nodes...)
	// Unique per install, so Portainer can tell d2k environments apart.
	if body["ID"] != "6f1c2a7e-0000-4000-8000-000000000001" {
		t.Errorf("ID = %v, want the namespace UID", body["ID"])
	}
	if body["NCPU"] != float64(6) || body["MemTotal"] != float64(12<<30) {
		t.Errorf("NCPU = %v, MemTotal = %v, want the cluster's allocatable 6 CPUs and 12Gi", body["NCPU"], body["MemTotal"])
	}

	// A ResourceQuota caps what the namespace can use.
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "q", Namespace: "d2k"},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceLimitsCPU:    resource.MustParse("2500m"),
			corev1.ResourceLimitsMemory: resource.MustParse("1Gi"),
		}},
	}
	body = info(t, append(nodes, quota)...)
	if body["NCPU"] != float64(2) || body["MemTotal"] != float64(1<<30) {
		t.Errorf("with quota: NCPU = %v, MemTotal = %v, want 2 and 1Gi", body["NCPU"], body["MemTotal"])
	}
}

// TestInfoSwarmOutsideSwarmMode checks /info outside Swarm mode reports an
// empty NodeID, as the daemon does: clients such as Portainer read a missing
// NodeID as a Swarm node.
func TestInfoSwarmOutsideSwarmMode(t *testing.T) {
	body := info(t)
	swarm, _ := body["Swarm"].(map[string]any)
	nodeID, ok := swarm["NodeID"]
	if !ok || nodeID != "" || swarm["LocalNodeState"] != "inactive" {
		t.Errorf("Swarm = %v, want NodeID \"\" and LocalNodeState inactive", body["Swarm"])
	}
}

func TestInfoCountsContainers(t *testing.T) {
	h, _ := newTestRouter()
	for _, name := range []string{"web", "db"} {
		if rec := do(t, h, "POST", "/containers/create?name="+name, `{"Image":"nginx"}`); rec.Code != http.StatusCreated {
			t.Fatalf("create %s = %d", name, rec.Code)
		}
	}
	var body map[string]any
	_ = json.Unmarshal(do(t, h, "GET", "/info", "").Body.Bytes(), &body)
	// Fake Deployments never become ready, so both count as stopped.
	if body["Containers"] != float64(2) || body["ContainersRunning"] != float64(0) || body["ContainersStopped"] != float64(2) {
		t.Errorf("Containers = %v running = %v stopped = %v, want 2, 0, 2", body["Containers"], body["ContainersRunning"], body["ContainersStopped"])
	}
}

func TestPingAndVersion(t *testing.T) {
	h, _ := newTestRouter()
	rec := do(t, h, "GET", "/_ping", "")
	if rec.Header().Get("OSType") != "linux" || rec.Header().Get("API-Version") == "" {
		t.Errorf("ping headers = %v, want OSType linux and API-Version", rec.Header())
	}
	var version map[string]any
	_ = json.Unmarshal(do(t, h, "GET", "/version", "").Body.Bytes(), &version)
	if version["GoVersion"] == nil || version["GoVersion"] == "" {
		t.Errorf("version = %v, want GoVersion", version)
	}
	// Portainer tells d2k from a Docker engine by the d2k component.
	components, _ := version["Components"].([]any)
	var names []any
	for _, c := range components {
		names = append(names, c.(map[string]any)["Name"])
	}
	if len(names) != 2 || names[0] != "Engine" || names[1] != "d2k" {
		t.Errorf("version Components = %v, want the Engine and d2k components", version["Components"])
	}
}
