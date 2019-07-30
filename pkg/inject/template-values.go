package inject

type (
	// Image contains the details to define a container image
	Image struct {
		Name       string
		PullPolicy string
		Version    string
	}

	// Port contains all the port-related setups
	Port struct {
		Admin               int32
		Control             int32
		Inbound             int32
		Outbound            int32
		IgnoreInboundPorts  string
		IgnoreOutboundPorts string
	}

	// Constraints wraps the Limit and Request settings for computational resources
	Constraints struct {
		Limit   string
		Request string
	}

	// Resources represents the computational resources setup for a given container
	Resources struct {
		CPU    Constraints
		Memory Constraints
	}

	// Identity contains the fields to set the identity variables in the proxy
	// sidecar container
	Identity struct {
		TrustDomain  string
		TrustAnchors string
	}

	// Proxy contains the fields to set the proxy sidecar container
	Proxy struct {
		Component             string
		ClusterDomain         string
		DisableIdentity       bool
		EnableExternalProfile bool
		HighAvailability      bool
		Identity              *Identity
		Image                 Image
		LogLevel              string
		ControlPlaneNamespace string
		Port                  Port
		UID                   int64
		ResourceRequirements  *Resources
	}

	// ProxyInit contains the fields to set the proxy-init container
	ProxyInit struct {
		Image                Image
		Port                 Port
		UID                  int64
		ResourceRequirements *Resources
	}
)
