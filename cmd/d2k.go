package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sethvargo/go-envconfig"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/internal/config"
	"github.com/portainer/d2k/internal/logging"
	"github.com/portainer/d2k/internal/pki"
	"github.com/portainer/d2k/internal/router"
	"github.com/portainer/d2k/internal/server"
)

const usage = `usage:
  d2k                              serve the Docker API (configured by D2K_* environment variables)
  d2k pki init --san NAME[,NAME] [--out DIR]
                                   create a CA, a server certificate valid for each NAME (DNS name
                                   or IP address clients use to reach d2k) and a client certificate
  d2k sleep SECONDS                sleep for SECONDS; the image has no shell or coreutils, so this
                                   is what the preStop hook in deploy/kubernetes.yaml runs`

func main() {
	if len(os.Args) > 1 {
		if len(os.Args) > 2 && os.Args[1] == "pki" && os.Args[2] == "init" {
			pkiInit(os.Args[3:])
			return
		}
		if len(os.Args) > 2 && os.Args[1] == "sleep" {
			sleep(os.Args[2])
			return
		}
		exitUsage()
	}
	serve()
}

func sleep(seconds string) {
	n, err := strconv.Atoi(seconds)
	if err != nil || n < 0 {
		fmt.Fprintf(os.Stderr, "sleep: invalid duration %q\n", seconds)
		os.Exit(2)
	}
	time.Sleep(time.Duration(n) * time.Second)
}

func exitUsage() {
	fmt.Fprintln(os.Stderr, usage)
	os.Exit(2)
}

func pkiInit(args []string) {
	fs := flag.NewFlagSet("pki init", flag.ExitOnError)
	sans := fs.String("san", "", "comma-separated DNS names and IP addresses for the server certificate")
	out := fs.String("out", "d2k-pki", "output directory")
	_ = fs.Parse(args)
	if *sans == "" {
		exitUsage()
	}
	if err := pki.Init(*out, strings.Split(*sans, ",")); err != nil {
		fmt.Fprintf(os.Stderr, "unable to create certificates: %s\n", err)
		os.Exit(1)
	}
	fmt.Printf(`Certificates written to %[1]s.

Load the server certificate into Kubernetes:
  kubectl -n <namespace> create secret generic d2k-tls --from-file=%[1]s/server

Point Docker clients at the client certificate:
  export DOCKER_HOST=tcp://<address>:2376 DOCKER_TLS_VERIFY=1 DOCKER_CERT_PATH=%[1]s/client

Keep %[1]s/ca.key private; it can sign certificates d2k trusts.
`, *out)
}

func serve() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var cfg config.Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		log.Fatal().Str("context", "Startup").Err(err).Msg("Unable to parse configuration")
	}
	if err := logging.Configure(cfg.LogLevel, cfg.LogFormat); err != nil {
		log.Fatal().Str("context", "Startup").Err(err).Msg("Unable to configure logging")
	}

	log.Info().Str("context", "Startup").Str("namespace", cfg.Namespace).Bool("swarm_mode", cfg.SwarmMode).Msg("Starting d2k")

	a, err := adapter.NewKubernetesDockerAdapter(&adapter.Options{Config: &cfg})
	if err != nil {
		log.Fatal().Str("context", "Startup").Err(err).Msg("Unable to create Kubernetes adapter")
	}
	log.Info().Str("context", "Startup").Str("namespace", cfg.Namespace).Msg("Connected to Kubernetes namespace")

	a.LogNFSStorageClasses(ctx)
	go a.RecordEvents(ctx)

	serverCfg := server.Config{
		Insecure:   cfg.Insecure,
		Port:       cfg.Port,
		TLSPort:    cfg.TLSPort,
		CertFile:   cfg.TLSCertFile,
		KeyFile:    cfg.TLSKeyFile,
		CAFile:     cfg.TLSCAFile,
		SocketPath: cfg.SocketPath,
		HealthPort: cfg.HealthPort,
	}
	if _, err := os.Stat(cfg.TLSCertFile); !cfg.Insecure && errors.Is(err, os.ErrNotExist) {
		// No d2k-tls Secret mounted: a new install, or an upgrade from a
		// release without mTLS. Use the Secret if it exists by now, or create
		// the certificates.
		material, created, err := a.EnsureTLS(ctx, cfg.TLSSANs)
		if err != nil {
			log.Fatal().Str("context", "Startup").Err(err).Msg("No TLS certificates mounted, and unable to create them (set D2K_INSECURE=true to serve without TLS)")
		}
		if created {
			log.Warn().Str("context", "Startup").Strs("names", cfg.TLSSANs).
				Msgf("Created d2k's certificates in Secrets %[1]s and %[2]s. Docker clients need the client certificate: kubectl -n %[3]s get secret %[2]s -o jsonpath='{.data.cert\\.pem}' | base64 -d > cert.pem (likewise key.pem and ca.pem)", adapter.TLSSecretName, adapter.ClientTLSSecretName, cfg.Namespace)
		}
		serverCfg.CertPEM, serverCfg.KeyPEM, serverCfg.CAPEM = material.Cert, material.Key, material.CA
	}

	handler := router.New(a, cfg.Namespace, cfg.SwarmMode)
	err = server.Run(ctx, serverCfg, handler)
	if err != nil {
		log.Fatal().Str("context", "Startup").Err(err).Msg("Server error")
	}
	log.Info().Str("context", "Startup").Msg("Stopped")
}
