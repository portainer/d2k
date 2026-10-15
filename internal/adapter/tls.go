package adapter

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/portainer/d2k/internal/pki"
)

// The Secrets that hold d2k's mTLS material: the server certificate, key
// and client CA (mounted by deploy/kubernetes.yaml), and the client
// certificate for Docker clients when d2k created them itself.
const (
	TLSSecretName       = "d2k-tls"
	ClientTLSSecretName = "d2k-client"
)

// TLSMaterial is d2k's server certificate, its key and the CA client
// certificates must chain to, PEM-encoded.
type TLSMaterial struct {
	Cert, Key, CA []byte
}

// EnsureTLS returns the mTLS material in the d2k-tls Secret, and reports
// whether it created it. Without that Secret it creates a CA, a server
// certificate for sans and the d2k Service's in-cluster names, and a
// client certificate: the server's in d2k-tls, the client's in d2k-client
// (the files DOCKER_CERT_PATH needs). The CA key is not kept: to change
// the names, delete both Secrets and restart d2k, then give clients the new
// client certificate. An admin's own d2k-tls Secret is used as it is.
func (a *KubernetesDockerAdapter) EnsureTLS(ctx context.Context, sans []string) (TLSMaterial, bool, error) {
	secrets := a.client.CoreV1().Secrets(a.namespace)
	if s, err := secrets.Get(ctx, TLSSecretName, metav1GetOptions()); err == nil {
		return tlsMaterial(s), false, nil
	} else if !errors.IsNotFound(err) {
		return TLSMaterial{}, false, fmt.Errorf("unable to read Secret %s: %w", TLSSecretName, err)
	}

	names := slices.Clone(sans)
	for _, n := range []string{"d2k", "d2k." + a.namespace, "d2k." + a.namespace + ".svc", "d2k." + a.namespace + ".svc.cluster.local", "localhost", "127.0.0.1"} {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	files, err := pki.Generate(names)
	if err != nil {
		return TLSMaterial{}, false, fmt.Errorf("unable to create certificates: %w", err)
	}
	server := tlsSecret(TLSSecretName, map[string][]byte{
		"tls.crt": files["server/tls.crt"], "tls.key": files["server/tls.key"], "ca.crt": files["server/ca.crt"],
	})
	created, err := secrets.Create(ctx, server, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		// Another d2k created it meanwhile: use that one.
		if s, err := secrets.Get(ctx, TLSSecretName, metav1GetOptions()); err == nil {
			return tlsMaterial(s), false, nil
		}
	}
	if err != nil {
		return TLSMaterial{}, false, fmt.Errorf("unable to create Secret %s: %w", TLSSecretName, err)
	}

	// A d2k-client left from earlier certificates no longer matches the CA.
	clientSecret := tlsSecret(ClientTLSSecretName, map[string][]byte{
		"ca.pem": files["client/ca.pem"], "cert.pem": files["client/cert.pem"], "key.pem": files["client/key.pem"],
	})
	if _, err := secrets.Create(ctx, clientSecret, metav1.CreateOptions{}); errors.IsAlreadyExists(err) {
		_, err = secrets.Update(ctx, clientSecret, metav1.UpdateOptions{})
		if err != nil {
			return TLSMaterial{}, false, fmt.Errorf("unable to update Secret %s: %w", ClientTLSSecretName, err)
		}
	} else if err != nil {
		return TLSMaterial{}, false, fmt.Errorf("unable to create Secret %s: %w", ClientTLSSecretName, err)
	}
	return tlsMaterial(created), true, nil
}

func tlsSecret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"app": "d2k"}},
		Data:       data,
	}
}

func tlsMaterial(s *corev1.Secret) TLSMaterial {
	return TLSMaterial{Cert: s.Data["tls.crt"], Key: s.Data["tls.key"], CA: s.Data["ca.crt"]}
}
