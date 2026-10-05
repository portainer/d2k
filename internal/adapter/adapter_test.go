package adapter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/portainer/d2k/internal/types"
)

const testNamespace = "d2k"

// newTestAdapter returns an adapter backed by a fake clientset seeded with objs.
func newTestAdapter(t *testing.T, objs ...runtime.Object) (*KubernetesDockerAdapter, *fake.Clientset) {
	t.Helper()
	client := fake.NewClientset(objs...)
	// Assign UIDs on create, as the API server does; container IDs derive from them.
	client.PrependReactor("create", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if obj, err := meta.Accessor(action.(k8stesting.CreateAction).GetObject()); err == nil && obj.GetUID() == "" {
			obj.SetUID(uuid.NewUUID())
		}
		return false, nil, nil
	})
	// Validate Service names as the API server does: they must be DNS-1035 labels.
	client.PrependReactor("create", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		svc := action.(k8stesting.CreateAction).GetObject().(*corev1.Service)
		if errs := validation.IsDNS1035Label(svc.Name); len(errs) > 0 {
			return true, nil, fmt.Errorf("Service %q is invalid: %s", svc.Name, strings.Join(errs, "; "))
		}
		return false, nil, nil
	})
	return NewWithClient(client, testNamespace, zap.NewNop().Sugar()), client
}

// deployment returns a Deployment in the test namespace with the given UID and labels.
func deployment(name, uid string, labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			UID:       k8stypes.UID(uid),
			Labels:    labels,
		},
	}
}

func managed() map[string]string {
	return map[string]string{types.LabelManagedBy: types.LabelManagedByValue}
}

func swarmManaged(service string) map[string]string {
	return map[string]string{
		types.LabelManagedBy:      types.LabelManagedByValue,
		types.LabelSwarmManagedBy: types.LabelSwarmManagedByValue,
		types.LabelSwarmService:   service,
	}
}

func TestReadClusterDNS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	conf := "search d2k.svc.cluster.local svc.cluster.local cluster.local\nnameserver 10.96.0.10\noptions ndots:5\n"
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if ns, domain := readClusterDNS(path); ns != "10.96.0.10" || domain != "cluster.local" {
		t.Fatalf("readClusterDNS() = %q, %q, want 10.96.0.10, cluster.local", ns, domain)
	}
	if ns, domain := readClusterDNS(filepath.Join(t.TempDir(), "missing")); ns != "" || domain != "" {
		t.Fatalf("readClusterDNS(missing) = %q, %q, want empty", ns, domain)
	}
}
