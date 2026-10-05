package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/portainer/d2k/internal/pki"
)

var ok = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("OK")) })

// newPKI writes a CA, server and client certificates for 127.0.0.1 to a temp dir.
func newPKI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := pki.Init(dir, []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// startMTLS serves ok with the mTLS configuration built from dir/server.
func startMTLS(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	cfg, err := mutualTLS(filepath.Join(dir, "server", "tls.crt"), filepath.Join(dir, "server", "tls.key"), filepath.Join(dir, "server", "ca.crt"), zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(ok)
	srv.TLS = cfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// client returns an HTTPS client that trusts the CA in dir and presents the
// client certificate in certDir, if any.
func client(t *testing.T, dir, certDir string, nextProtos ...string) *http.Client {
	t.Helper()
	ca, err := os.ReadFile(filepath.Join(dir, "client", "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	cfg := &tls.Config{RootCAs: roots, NextProtos: nextProtos}
	if certDir != "" {
		cert, err := tls.LoadX509KeyPair(filepath.Join(certDir, "client", "cert.pem"), filepath.Join(certDir, "client", "key.pem"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: len(nextProtos) > 0}}
}

func TestMutualTLS(t *testing.T) {
	dir := newPKI(t)
	other := newPKI(t)
	srv := startMTLS(t, dir)

	if _, err := client(t, dir, "").Get(srv.URL); err == nil {
		t.Error("request without a client certificate succeeded")
	}
	if _, err := client(t, dir, other).Get(srv.URL); err == nil {
		t.Error("request with a client certificate from another CA succeeded")
	}
	resp, err := client(t, dir, dir).Get(srv.URL)
	if err != nil {
		t.Fatalf("request with a valid client certificate failed: %v", err)
	}
	_ = resp.Body.Close()

	// HTTP/2 would break connection hijacking for exec and attach.
	resp, err = client(t, dir, dir, "h2", "http/1.1").Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.ProtoMajor != 1 {
		t.Errorf("negotiated %s, want HTTP/1.1", resp.Proto)
	}
}

func TestMutualTLSReloadsRotatedMaterial(t *testing.T) {
	dir := newPKI(t)
	srv := startMTLS(t, dir)

	// Rotate to a new CA and certificates in place, as a Secret update does.
	rotated := newPKI(t)
	later := time.Now().Add(time.Minute)
	for _, f := range []string{"tls.crt", "tls.key", "ca.crt"} {
		data, err := os.ReadFile(filepath.Join(rotated, "server", f))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "server", f)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, later, later); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := client(t, rotated, rotated).Get(srv.URL)
	if err != nil {
		t.Fatalf("rotated client certificate rejected: %v", err)
	}
	_ = resp.Body.Close()
	if _, err := client(t, rotated, dir).Get(srv.URL); err == nil {
		t.Error("client certificate from the replaced CA still accepted")
	}
}

func TestRunRequiresTLSMaterial(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	err := Run(context.Background(), Config{
		TLSPort: freePort(t), HealthPort: freePort(t),
		CertFile: missing, KeyFile: missing, CAFile: missing,
	}, ok, zap.NewNop().Sugar())
	if err == nil {
		t.Fatal("Run() without certificates succeeded")
	}
}

func TestRunServesHealthAndSocket(t *testing.T) {
	cfg := Config{
		Insecure:   true,
		Port:       freePort(t),
		HealthPort: freePort(t),
		SocketPath: filepath.Join(t.TempDir(), "docker.sock"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- Run(ctx, cfg, ok, zap.NewNop().Sugar()) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})

	if body := get(t, http.DefaultClient, fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.HealthPort)); body != "ok" {
		t.Errorf("healthz = %q, want ok", body)
	}
	// The health port must not expose the Docker API.
	if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/_ping", cfg.HealthPort)); err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("health port served /_ping: %d", resp.StatusCode)
		}
	}

	unix := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", cfg.SocketPath)
	}}}
	if body := get(t, unix, "http://docker/_ping"); body != "OK" {
		t.Errorf("unix socket /_ping = %q, want OK", body)
	}
	if info, err := os.Stat(cfg.SocketPath); err != nil || info.Mode().Perm() != 0o660 {
		t.Errorf("socket mode = %v, want 0660", info.Mode().Perm())
	}
}

// get retries until the listener is up.
func get(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := c.Get(url)
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			return string(body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s: %v", url, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}
