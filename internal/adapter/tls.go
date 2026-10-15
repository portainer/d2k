package adapter

import (
	"context"
	"errors"
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
// whether it created it.
func (a *KubernetesDockerAdapter) EnsureTLS(ctx context.Context, sans []string) (TLSMaterial, bool, error) {
	return TLSMaterial{}, false, errors.New("not implemented")
}
