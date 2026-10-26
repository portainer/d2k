package adapter

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"maps"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

func parseCert(t *testing.T, data []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("no PEM certificate in %q", data)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// Without a d2k-tls Secret (a new install, or an upgrade from a release
// that served plain TCP), d2k creates its certificates instead of refusing
// to start: the server's in d2k-tls, a client certificate for Docker
// clients in d2k-client. The CA key is not kept anywhere.
func TestEnsureTLSCreatesTheSecrets(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()

	got, created, err := a.EnsureTLS(ctx, []string{"d2k.example.com", "10.0.0.5"})
	if err != nil || !created {
		t.Fatalf("EnsureTLS = %v, %v, want the Secrets created", created, err)
	}
	server, err := client.CoreV1().Secrets(testNamespace).Get(ctx, TLSSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(server.Data)); !slices.Equal(keys, []string{"ca.crt", "tls.crt", "tls.key"}) {
		t.Errorf("d2k-tls keys = %v, want ca.crt, tls.crt, tls.key only (no CA key)", keys)
	}
	if !bytes.Equal(server.Data["tls.crt"], got.Cert) || !bytes.Equal(server.Data["ca.crt"], got.CA) {
		t.Error("EnsureTLS returned other material than it stored")
	}
	cert := parseCert(t, got.Cert)
	for _, name := range []string{"d2k.example.com", "d2k", "d2k." + testNamespace + ".svc", "d2k." + testNamespace + ".svc.cluster.local", "localhost"} {
		if !slices.Contains(cert.DNSNames, name) {
			t.Errorf("server certificate DNS names = %v, want %s", cert.DNSNames, name)
		}
	}
	if len(cert.IPAddresses) == 0 || cert.IPAddresses[0].String() != "10.0.0.5" {
		t.Errorf("server certificate IPs = %v, want 10.0.0.5 first", cert.IPAddresses)
	}

	clientSecret, err := client.CoreV1().Secrets(testNamespace).Get(ctx, ClientTLSSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(clientSecret.Data)); !slices.Equal(keys, []string{"ca.pem", "cert.pem", "key.pem"}) {
		t.Errorf("d2k-client keys = %v, want the DOCKER_CERT_PATH files", keys)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(got.CA)
	if _, err := parseCert(t, clientSecret.Data["cert.pem"]).Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("client certificate doesn't chain to the CA d2k trusts: %v", err)
	}

	again, created, err := a.EnsureTLS(ctx, []string{"other.example.com"})
	if err != nil || created || !bytes.Equal(again.Cert, got.Cert) {
		t.Errorf("second EnsureTLS = created %v, %v: want the stored material, unchanged", created, err)
	}
}

// An admin's own d2k-tls Secret (d2k pki init, cert-manager) is used as it
// is, and d2k creates no client certificate.
func TestEnsureTLSUsesAnExistingSecret(t *testing.T) {
	own := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: TLSSecretName, Namespace: testNamespace},
		Data: map[string][]byte{"tls.crt": []byte("cert"), "tls.key": []byte("key"), "ca.crt": []byte("ca")}}
	a, client := newTestAdapter(t, own)
	got, created, err := a.EnsureTLS(context.Background(), []string{"d2k.example.com"})
	if err != nil || created || string(got.Cert) != "cert" || string(got.Key) != "key" || string(got.CA) != "ca" {
		t.Errorf("EnsureTLS = %+v, created %v, %v: want the admin's Secret", got, created, err)
	}
	if _, err := client.CoreV1().Secrets(testNamespace).Get(context.Background(), ClientTLSSecretName, metav1.GetOptions{}); err == nil {
		t.Error("d2k created a client certificate beside the admin's own")
	}
}

// A start that created d2k-tls but failed to write d2k-client doesn't leave
// d2k without a client certificate for good: the next start sees d2k's own
// d2k-tls without its d2k-client, and creates both anew.
func TestEnsureTLSRecoversAFailedFirstStart(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	failed := false
	client.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		s := action.(k8stesting.CreateAction).GetObject().(*corev1.Secret)
		if s.Name == ClientTLSSecretName && !failed {
			failed = true
			return true, nil, fmt.Errorf("the API server is unavailable")
		}
		return false, nil, nil
	})

	if _, _, err := a.EnsureTLS(ctx, nil); err == nil {
		t.Fatal("EnsureTLS succeeded although d2k-client couldn't be written")
	}

	got, created, err := a.EnsureTLS(ctx, nil)
	if err != nil || !created {
		t.Fatalf("second EnsureTLS = created %v, %v: want the certificates created anew", created, err)
	}
	clientSecret, err := client.CoreV1().Secrets(testNamespace).Get(ctx, ClientTLSSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("no d2k-client after the second start: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(got.CA)
	if _, err := parseCert(t, clientSecret.Data["cert.pem"]).Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("client certificate doesn't chain to the CA d2k serves: %v", err)
	}
	server, _ := client.CoreV1().Secrets(testNamespace).Get(ctx, TLSSecretName, metav1.GetOptions{})
	if !bytes.Equal(server.Data["ca.crt"], got.CA) {
		t.Error("d2k-tls holds another CA than the one EnsureTLS returned")
	}

	// With both Secrets there, later starts use them as they are.
	again, created, err := a.EnsureTLS(ctx, nil)
	if err != nil || created || !bytes.Equal(again.CA, got.CA) {
		t.Errorf("third EnsureTLS = created %v, %v: want the stored material", created, err)
	}
}
