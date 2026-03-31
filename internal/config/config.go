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

	// Port is the port on which the Docker-compatible API server listens.
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
}
