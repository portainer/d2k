package pki

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

func TestInit(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, []string{"d2k.example.com", "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	ca, err := os.ReadFile(filepath.Join(dir, "client", "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	roots.AppendCertsFromPEM(ca)

	server, err := tls.LoadX509KeyPair(filepath.Join(dir, "server", "tls.crt"), filepath.Join(dir, "server", "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"d2k.example.com", "127.0.0.1"} {
		if _, err := server.Leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Errorf("server certificate not valid for %s: %v", host, err)
		}
	}
	if _, err := server.Leaf.Verify(x509.VerifyOptions{DNSName: "other.example.com", Roots: roots}); err == nil {
		t.Error("server certificate valid for a name it was not issued for")
	}

	client, err := tls.LoadX509KeyPair(filepath.Join(dir, "client", "cert.pem"), filepath.Join(dir, "client", "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("client certificate not valid for client auth: %v", err)
	}
	if _, err := client.Leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Error("client certificate usable as a server certificate")
	}

	info, err := os.Stat(filepath.Join(dir, "ca.key"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("ca.key mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestInitRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, []string{"localhost"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err := Init(dir, []string{"localhost"}); err == nil {
		t.Fatal("Init() overwrote an existing CA")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "ca.key"))
	if string(before) != string(after) {
		t.Fatal("CA key changed")
	}
}

func TestInitRequiresSANs(t *testing.T) {
	if err := Init(t.TempDir(), nil); err == nil {
		t.Fatal("Init() without SANs succeeded")
	}
}

func TestInitNeverWritesAKeyOthersCanRead(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "client", "key.pem")
	if err := os.MkdirAll(filepath.Dir(key), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}

	err := Init(dir, []string{"localhost"})
	data, _ := os.ReadFile(key)
	info, _ := os.Stat(key)
	if len(data) > 0 && info.Mode().Perm()&0o077 != 0 {
		t.Errorf("client key written with mode %v", info.Mode().Perm())
	}
	if err == nil {
		t.Error("Init() overwrote an existing client key")
	}
}

func TestInitSANs(t *testing.T) {
	// A space after a comma is how people type lists: it is not part of the name.
	dir := t.TempDir()
	if err := Init(dir, []string{"a.example.com", " b.example.com"}); err != nil {
		t.Fatal(err)
	}
	server, err := tls.LoadX509KeyPair(filepath.Join(dir, "server", "tls.crt"), filepath.Join(dir, "server", "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Leaf.VerifyHostname("b.example.com"); err != nil {
		t.Errorf("server certificate not valid for b.example.com: %v", err)
	}

	// Names no client can match fail now, not at the first connection.
	for _, san := range []string{"", "d2k.example.com:2376", "tcp://d2k.example.com", "[::1]"} {
		if err := Init(t.TempDir(), []string{san}); err == nil {
			t.Errorf("Init() accepted SAN %q", san)
		}
	}
}
