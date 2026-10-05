// Package server runs d2k's listeners: the Docker API over mutual TLS (or
// plain TCP when explicitly insecure), an optional unix socket, and a plain
// health endpoint for kubelet probes.
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// Config selects and configures the listeners.
type Config struct {
	// Insecure serves the Docker API over plain TCP on Port. Otherwise it is
	// served over mutual TLS on TLSPort and the certificate files are required.
	Insecure bool
	Port     int
	TLSPort  int

	CertFile string // server certificate
	KeyFile  string // server key
	CAFile   string // CA that client certificates must chain to

	// SocketPath, when set, also serves the Docker API on a unix socket,
	// protected by file permissions rather than TLS.
	SocketPath string

	// HealthPort serves GET /healthz over plain HTTP.
	HealthPort int
}

// readHeaderTimeout bounds the request header only: exec, attach, logs and
// events streams stay open for as long as the client wants.
const readHeaderTimeout = 30 * time.Second

// Run serves handler on the configured listeners until ctx is cancelled,
// then shuts them down gracefully. It fails before serving anything when the
// TLS material cannot be loaded.
func Run(ctx context.Context, cfg Config, handler http.Handler) error {
	var servers []*http.Server
	var listeners []net.Listener

	api := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		// A non-nil, empty map disables HTTP/2: Docker's attach and exec
		// endpoints hijack the HTTP/1.1 connection.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
	}
	if cfg.Insecure {
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Port))
		if err != nil {
			return err
		}
		log.Warn().Str("context", "Server").Any("addr", l.Addr().String()).Msg("D2K_INSECURE is set: serving the Docker API without TLS or authentication")
		servers, listeners = append(servers, api), append(listeners, l)
	} else {
		tlsConfig, err := mutualTLS(cfg.CertFile, cfg.KeyFile, cfg.CAFile)
		if err != nil {
			return fmt.Errorf("mTLS is required (set D2K_INSECURE=true to serve without TLS): %w", err)
		}
		l, err := tls.Listen("tcp", fmt.Sprintf(":%d", cfg.TLSPort), tlsConfig)
		if err != nil {
			return err
		}
		log.Info().Str("context", "Server").Any("addr", l.Addr().String()).Msg("Serving the Docker API over mutual TLS")
		servers, listeners = append(servers, api), append(listeners, l)
	}

	if cfg.SocketPath != "" {
		l, err := listenUnix(cfg.SocketPath)
		if err != nil {
			return err
		}
		log.Info().Str("context", "Server").Any("path", cfg.SocketPath).Msg("Serving the Docker API on a unix socket")
		sock := &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}
		servers, listeners = append(servers, sock), append(listeners, l)
	}

	health := http.NewServeMux()
	health.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.HealthPort))
	if err != nil {
		return err
	}
	servers = append(servers, &http.Server{Handler: health, ReadHeaderTimeout: readHeaderTimeout})
	listeners = append(listeners, l)

	errs := make(chan error, len(servers))
	for i, srv := range servers {
		go func() {
			if err := srv.Serve(listeners[i]); !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		}()
	}

	select {
	case err := <-errs:
		shutdown(servers)
		return err
	case <-ctx.Done():
		shutdown(servers)
		return nil
	}
}

func shutdown(servers []*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(ctx)
	}
}

// listenUnix listens on path, replacing a stale socket from a previous run.
func listenUnix(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

// mutualTLS returns a TLS configuration that requires a client certificate
// signed by the CA in caFile. The files are reloaded when they change, so a
// rotated Kubernetes Secret takes effect without a restart.
func mutualTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	r := &reloader{certFile: certFile, keyFile: keyFile, caFile: caFile}
	if err := r.load(); err != nil {
		return nil, err
	}
	base := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientAuth: tls.RequireAndVerifyClientCert,
		NextProtos: []string{"http/1.1"},
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		// Built per handshake so the current certificate and CA are used.
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			cert, pool := r.current()
			cfg := base.Clone()
			cfg.Certificates = []tls.Certificate{cert}
			cfg.ClientCAs = pool
			return cfg, nil
		},
	}, nil
}

// reloader holds the server certificate and client CA, reloading them when
// any of the files changes.
type reloader struct {
	certFile, keyFile, caFile string

	mu      sync.Mutex
	cert    tls.Certificate
	pool    *x509.CertPool
	modTime time.Time
}

func (r *reloader) load() error {
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("load server certificate: %w", err)
	}
	ca, err := os.ReadFile(r.caFile)
	if err != nil {
		return fmt.Errorf("load client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return fmt.Errorf("load client CA: no certificates in %s", r.caFile)
	}
	r.cert, r.pool, r.modTime = cert, pool, r.latestModTime()
	return nil
}

func (r *reloader) latestModTime() time.Time {
	var latest time.Time
	for _, f := range []string{r.certFile, r.keyFile, r.caFile} {
		if info, err := os.Stat(f); err == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest
}

// current returns the certificate and CA, reloading them first if a file
// changed. A failed reload keeps serving the previous material.
func (r *reloader) current() (tls.Certificate, *x509.CertPool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if latest := r.latestModTime(); latest.After(r.modTime) {
		if err := r.load(); err != nil {
			// Retry on the next change, not on every handshake.
			r.modTime = latest
			log.Error().Str("context", "Server").Err(err).Msg("Unable to reload TLS material, keeping the previous one")
		} else {
			log.Info().Str("context", "Server").Msg("Reloaded TLS material")
		}
	}
	return r.cert, r.pool
}
