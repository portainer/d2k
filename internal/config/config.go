package config

// Config represents the configuration of the d2k application.
type Config struct {
	// KubeConfigPath is the path to the kubeconfig file used to connect to the Kubernetes cluster.
	// If empty, in-cluster config is used (i.e. d2k is running inside the target cluster).
	// Provided via D2K_KUBECONFIG env var.
	KubeConfigPath string `env:"D2K_KUBECONFIG"`

	// Namespace is the target Kubernetes namespace that d2k will manage.
	// All Docker API operations are scoped to this single namespace.
	// Provided via D2K_NAMESPACE env var, defaults to "default".
	Namespace string `env:"D2K_NAMESPACE,default=default"`

	// Port is the plain TCP port of the Docker API, used only with Insecure.
	// Provided via D2K_PORT env var, defaults to 2375.
	Port int `env:"D2K_PORT,default=2375"`

	// LogLevel is the log level for the application.
	// Valid values: debug, info, warn, error.
	// Provided via D2K_LOG_LEVEL env var, defaults to info.
	LogLevel string `env:"D2K_LOG_LEVEL,default=info"`

	// LogFormat is the log format for the application.
	// Valid values: text, json.
	// Provided via D2K_LOG_FORMAT env var, defaults to text.
	LogFormat string `env:"D2K_LOG_FORMAT,default=text"`

	// LowPortThreshold is the port number below which a LoadBalancer Service is created
	// instead of a NodePort Service for explicit port mappings.
	// Provided via D2K_LOW_PORT_THRESHOLD env var, defaults to 1024.
	LowPortThreshold int `env:"D2K_LOW_PORT_THRESHOLD,default=1024"`

	// GPUResourceName is the Kubernetes device plugin resource name used when
	// the Docker client requests GPU access (--gpus flag / DeviceRequests).
	// Common values: "nvidia.com/gpu" (NVIDIA), "amd.com/gpu" (AMD/ROCm).
	// When empty, GPU requests from the Docker API are silently ignored.
	// Provided via D2K_GPU_RESOURCE_NAME env var, empty by default.
	GPUResourceName string `env:"D2K_GPU_RESOURCE_NAME"`

	// SwarmMode enables the Docker Swarm API surface in addition to the standard
	// Docker Engine API. When true, d2k also handles /swarm, /nodes, /services,
	// /tasks, /secrets, and /configs endpoints, translating Swarm operations to
	// Kubernetes equivalents in the configured namespace.
	// Provided via D2K_SWARM_MODE env var, defaults to false.
	SwarmMode bool `env:"D2K_SWARM_MODE,default=false"`

	// Insecure serves the Docker API over plain TCP on Port, without TLS or
	// client authentication. By default d2k requires mutual TLS on TLSPort and
	// refuses to start without its certificates.
	// Provided via D2K_INSECURE env var, defaults to false.
	Insecure bool `env:"D2K_INSECURE,default=false"`

	// TLSCertFile is the path to the server certificate.
	// Defaults to /etc/d2k/tls/tls.crt (standard Kubernetes Secret mount path).
	TLSCertFile string `env:"D2K_TLS_CERT_FILE,default=/etc/d2k/tls/tls.crt"`

	// TLSKeyFile is the path to the TLS private key file.
	// Defaults to /etc/d2k/tls/tls.key (standard Kubernetes Secret mount path).
	TLSKeyFile string `env:"D2K_TLS_KEY_FILE,default=/etc/d2k/tls/tls.key"`

	// TLSCAFile is the path to the CA certificate that client certificates
	// must be signed by.
	// Defaults to /etc/d2k/tls/ca.crt (standard Kubernetes Secret mount path).
	TLSCAFile string `env:"D2K_TLS_CA_FILE,default=/etc/d2k/tls/ca.crt"`

	// TLSPort is the port of the mutual TLS Docker API.
	// Defaults to 2376 (standard Docker TLS port).
	TLSPort int `env:"D2K_TLS_PORT,default=2376"`

	// SocketPath, when set, also serves the Docker API on this unix socket.
	// Provided via D2K_SOCKET env var, empty by default.
	SocketPath string `env:"D2K_SOCKET"`

	// HealthPort serves GET /healthz over plain HTTP for kubelet probes.
	// Provided via D2K_HEALTH_PORT env var, defaults to 8081.
	HealthPort int `env:"D2K_HEALTH_PORT,default=8081"`
}
