package types

const (
	// Version is the d2k server version, reported in Docker API version responses.
	Version = "0.1.0"

	// DockerAPIVersion is the Docker API version we claim to implement.
	DockerAPIVersion = "1.41"

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

	// ServiceTypeLB is the value for LabelServiceType when a LoadBalancer Service was created.
	ServiceTypeLB = "loadbalancer"

	// ServiceTypeNodePort is the value for LabelServiceType when a NodePort Service was created.
	ServiceTypeNodePort = "nodeport"

	// ServiceTypeNone is the value for LabelServiceType when no Service was created.
	ServiceTypeNone = "none"
)
