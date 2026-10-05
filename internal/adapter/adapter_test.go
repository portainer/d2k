package adapter

import (
	"testing"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/portainer/d2k/internal/types"
)

const testNamespace = "d2k"

// newTestAdapter returns an adapter backed by a fake clientset seeded with objs.
func newTestAdapter(t *testing.T, objs ...runtime.Object) (*KubernetesDockerAdapter, *fake.Clientset) {
	t.Helper()
	client := fake.NewClientset(objs...)
	return &KubernetesDockerAdapter{
		client:            client,
		namespace:         testNamespace,
		logger:            zap.NewNop().Sugar(),
		prevCPU:           map[string]int64{},
		networks:          map[string]*NetworkSummary{},
		nfsStorageClasses: map[string]string{},
	}, client
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
