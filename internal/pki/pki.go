// Package pki creates the certificates d2k's mTLS endpoint needs: a CA, a
// server certificate signed by it, and a client certificate for Docker
// clients such as the Docker CLI and Portainer.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 2 * 365 * 24 * time.Hour
)

// Init writes a new CA and certificates to dir:
//
//	ca.crt, ca.key                     the CA; keep ca.key private
//	server/tls.crt, tls.key, ca.crt    for d2k (load into the d2k-tls Secret)
//	client/ca.pem, cert.pem, key.pem   for Docker clients (DOCKER_CERT_PATH)
//
// sans are the DNS names and IP addresses clients use to reach d2k. Init
// refuses to overwrite an existing CA.
func Init(dir string, sans []string) error {
	if len(sans) == 0 {
		return errors.New("at least one subject alternative name is required")
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.key")); err == nil {
		return fmt.Errorf("%s already holds a CA; refusing to overwrite it", dir)
	}

	caKey, caCert, err := newCertificate(&x509.Certificate{
		Subject:               pkix.Name{CommonName: "d2k CA"},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		NotAfter:              time.Now().Add(caValidity),
	}, nil, nil)
	if err != nil {
		return err
	}

	server := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "d2k"},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		NotAfter:    time.Now().Add(leafValidity),
	}
	for _, san := range sans {
		if ip := net.ParseIP(san); ip != nil {
			server.IPAddresses = append(server.IPAddresses, ip)
		} else {
			server.DNSNames = append(server.DNSNames, san)
		}
	}
	serverKey, serverCert, err := newCertificate(server, caCert, caKey)
	if err != nil {
		return err
	}

	clientKey, clientCert, err := newCertificate(&x509.Certificate{
		Subject:     pkix.Name{CommonName: "d2k client"},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		NotAfter:    time.Now().Add(leafValidity),
	}, caCert, caKey)
	if err != nil {
		return err
	}

	files := map[string][]byte{
		"ca.crt":          encodeCert(caCert),
		"ca.key":          encodeKey(caKey),
		"server/tls.crt":  encodeCert(serverCert),
		"server/tls.key":  encodeKey(serverKey),
		"server/ca.crt":   encodeCert(caCert),
		"client/ca.pem":   encodeCert(caCert),
		"client/cert.pem": encodeCert(clientCert),
		"client/key.pem":  encodeKey(clientKey),
	}
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// newCertificate creates a key and a certificate from template, signed by
// parent, or self-signed when parent is nil.
func newCertificate(template, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	template.SerialNumber = serial
	template.NotBefore = time.Now().Add(-time.Hour)
	if parent == nil {
		parent, parentKey = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, parentKey)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return key, cert, err
}

func encodeCert(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

func encodeKey(key *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		panic(err) // only fails for keys on unsupported curves
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}
