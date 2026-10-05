package types

// Version is the d2k server version, reported in Docker API version
// responses. Release builds set it with
// -ldflags "-X github.com/portainer/d2k/internal/types.Version=<version>".
var Version = "dev"

const (
	// LabelPrefix is the prefix for all labels d2k places on Kubernetes resources.
	LabelPrefix = "d2k.portainer.io"

	// LabelWorkloadName is the label key storing the original Docker container name.
	LabelWorkloadName = LabelPrefix + "/name"

	// LabelManagedBy marks resources created and owned by d2k.
	LabelManagedBy = LabelPrefix + "/managed-by"

	// LabelManagedByValue is the value used for LabelManagedBy.
	LabelManagedByValue = "d2k"

	// LabelServiceType records how the Service for this workload was created.
	// Values: "loadbalancer", "nodeport", "none"
	LabelServiceType = LabelPrefix + "/service-type"

	// AnnotationPortMappings stores the original Docker port mapping string on the Deployment
	// as an annotation (not a label) because the JSON-encoded value contains characters such
	// as ':' and '[' that are illegal in Kubernetes label values.
	AnnotationPortMappings = LabelPrefix + "/port-mappings"

	// LabelImageRef stores the original image reference as supplied by the Docker client.
	AnnotationImageRef = LabelPrefix + "/image-ref"

	// AnnotationDockerName stores the Docker container name. It differs from the
	// Deployment name after a rename, since Kubernetes object names are immutable.
	AnnotationDockerName = LabelPrefix + "/docker-name"

	// AnnotationDockerLabels stores the Docker labels verbatim as JSON. Kubernetes
	// label values are limited to 63 characters from a restricted charset, so
	// labels such as com.docker.compose.config-hash cannot round-trip as labels.
	AnnotationDockerLabels = LabelPrefix + "/docker-labels"

	// AnnotationDriverOpts stores a volume's Docker driver options as JSON.
	AnnotationDriverOpts = LabelPrefix + "/driver-opts"

	// AnnotationVolumeNames maps the PVCs a container mounts to their Docker
	// volume names, as JSON.
	AnnotationVolumeNames = LabelPrefix + "/volume-names"

	// AnnotationStartedAt and AnnotationStoppedAt record when the container was
	// last started and stopped (RFC 3339). A container never started is in
	// Docker's "created" state; a stopped one is "exited".
	AnnotationStartedAt = LabelPrefix + "/started-at"
	AnnotationStoppedAt = LabelPrefix + "/stopped-at"

	// AnnotationNetworks stores the names of the Docker networks a container is
	// attached to, as JSON. Compose compares them on every `up`.
	AnnotationNetworks = LabelPrefix + "/networks"

	// LabelComposeProject marks the pods of a compose project, selected by the
	// project's DNS Service so service names resolve within the project only.
	LabelComposeProject = LabelPrefix + "/compose-project"

	// ServiceTypeLB is the value for LabelServiceType when a LoadBalancer Service was created.
	ServiceTypeLB = "loadbalancer"

	// ServiceTypeNodePort is the value for LabelServiceType when a NodePort Service was created.
	ServiceTypeNodePort = "nodeport"

	// ServiceTypeNone is the value for LabelServiceType when no Service was created.
	ServiceTypeNone = "none"

	// d2k (Swarm mode) label constants.
	// These are placed on all Kubernetes resources created via the Swarm API surface.

	// LabelSwarmStack is the Swarm stack name a resource belongs to.
	LabelSwarmStack = LabelPrefix + "/swarm-stack"

	// LabelSwarmService is the Swarm service name within a stack.
	LabelSwarmService = LabelPrefix + "/swarm-service"

	// LabelSwarmManagedBy marks resources owned by the Swarm emulation layer.
	LabelSwarmManagedBy = LabelPrefix + "/swarm-managed-by"

	// LabelSwarmManagedByValue is the value used for LabelSwarmManagedBy.
	LabelSwarmManagedByValue = "d2k"

	// AnnotationSwarmServiceID stores the deterministic Swarm-format service ID
	// derived from the Kubernetes resource UID.
	AnnotationSwarmServiceID = LabelPrefix + "/swarm-service-id"

	// AnnotationSwarmNodeID stores the deterministic Swarm-format node ID
	// derived from the Kubernetes node UID.
	AnnotationSwarmNodeID = LabelPrefix + "/swarm-node-id"

	// ConfigMapSwarmIdentity is the name of the ConfigMap in the namespace that
	// stores the stable Swarm cluster identity (swarm ID, manager node ID, etc.)
	// derived once from the cluster UID and persisted across d2k restarts.
	ConfigMapSwarmIdentity = "d2k-identity"

	// AnnotationEndpointMode stores the Swarm endpoint mode for a service.
	// Values: "vip" (default) or "dnsrr" (host-port / direct node IP mode).
	AnnotationEndpointMode = LabelPrefix + "/endpoint-mode"
)
