package kubevirt

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rsturla/openshell-drivers-contrib/pkg/bootstrap"
	"k8s.io/apimachinery/pkg/api/resource"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
)

// Config holds the configuration for the KubeVirt compute driver.
type Config struct {
	Namespace           string            // K8s namespace for VMs
	BootSource          string            // DataSource name (e.g. "fedora", "rhel9")
	BootSourceNamespace string            // DataSource namespace (e.g. "openshift-virtualization-os-images")
	DefaultInstanceType string            // VirtualMachineClusterInstancetype name (e.g. "cx1.4xlarge")
	DefaultPreference   string            // optional VirtualMachineClusterPreference name
	StorageClass        string            // required allowlisted encrypted StorageClass
	StorageLabels       map[string]string // extra labels applied to PVC-creating resources (e.g. appcode)
	StorageAnnotations  map[string]string // extra annotations applied to PVC-creating resources (e.g. reclaimPolicy)
	Transport           bootstrap.TransportConfig
	GatewayID           string
	DiskSize            string        // Root disk size (e.g. "100Gi")
	PollInterval        time.Duration // Backstop reap interval
	MaxInstances        int
	MaxInstanceAge      time.Duration
}

// DefaultConfig returns sensible operational defaults for the KubeVirt driver.
func DefaultConfig() Config {
	return Config{
		Namespace:           "openshell",
		BootSource:          "fedora",
		BootSourceNamespace: "openshift-virtualization-os-images",
		DefaultInstanceType: "cx1.4xlarge",
		DiskSize:            "100Gi",
		PollInterval:        15 * time.Second,
		MaxInstances:        10,
		MaxInstanceAge:      8 * time.Hour,
	}
}

// Validate checks that all required configuration fields are set and
// well-formed. It uses errors.Join for multi-error reporting.
func (c *Config) Validate() error {
	var errs []error

	if c.Namespace == "" {
		errs = append(errs, fmt.Errorf("namespace is required"))
	} else if len(utilvalidation.IsDNS1123Label(c.Namespace)) != 0 {
		errs = append(errs, fmt.Errorf("namespace must be a valid Kubernetes namespace name"))
	}
	if c.BootSource == "" {
		errs = append(errs, fmt.Errorf("bootSource is required"))
	} else if len(utilvalidation.IsDNS1123Subdomain(c.BootSource)) != 0 {
		errs = append(errs, fmt.Errorf("bootSource must be a valid Kubernetes resource name"))
	}
	if c.BootSourceNamespace == "" {
		errs = append(errs, fmt.Errorf("bootSourceNamespace is required"))
	} else if len(utilvalidation.IsDNS1123Label(c.BootSourceNamespace)) != 0 {
		errs = append(errs, fmt.Errorf("bootSourceNamespace must be a valid Kubernetes namespace name"))
	}
	if c.DefaultInstanceType == "" {
		errs = append(errs, fmt.Errorf("defaultInstanceType is required"))
	} else if len(utilvalidation.IsDNS1123Subdomain(c.DefaultInstanceType)) != 0 {
		errs = append(errs, fmt.Errorf("defaultInstanceType contains invalid characters"))
	}
	if c.DefaultPreference != "" && len(utilvalidation.IsDNS1123Subdomain(c.DefaultPreference)) != 0 {
		errs = append(errs, fmt.Errorf("defaultPreference must be a valid Kubernetes resource name"))
	}
	if c.StorageClass == "" {
		errs = append(errs, fmt.Errorf("storageClass is required and must provide encrypted disposable storage"))
	} else if len(utilvalidation.IsDNS1123Subdomain(c.StorageClass)) != 0 {
		errs = append(errs, fmt.Errorf("storageClass must be a valid Kubernetes resource name"))
	}
	if err := c.Transport.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("transport: %w", err))
	}
	if c.GatewayID == "" {
		errs = append(errs, fmt.Errorf("gatewayID is required"))
	} else if len(c.GatewayID) > 256 {
		errs = append(errs, fmt.Errorf("gatewayID must not exceed 256 bytes"))
	}
	if c.DiskSize == "" {
		errs = append(errs, fmt.Errorf("diskSize is required"))
	} else if quantity, err := resource.ParseQuantity(c.DiskSize); err != nil {
		errs = append(errs, fmt.Errorf("diskSize must be a valid Kubernetes quantity: %w", err))
	} else if quantity.Cmp(resource.MustParse("1Gi")) < 0 || quantity.Cmp(resource.MustParse("16Ti")) > 0 {
		errs = append(errs, fmt.Errorf("diskSize must be between 1Gi and 16Ti"))
	}
	if c.PollInterval <= 0 || c.PollInterval > time.Hour {
		errs = append(errs, fmt.Errorf("pollInterval must be greater than zero and at most 1h"))
	}
	if c.MaxInstances <= 0 || c.MaxInstances > 1000 {
		errs = append(errs, fmt.Errorf("maxInstances must be between 1 and 1000"))
	}
	if c.MaxInstanceAge <= 0 || c.MaxInstanceAge > 30*24*time.Hour {
		errs = append(errs, fmt.Errorf("maxInstanceAge must be greater than zero and at most 720h"))
	}

	for field, value := range map[string]string{
		"Namespace": c.Namespace, "BootSource": c.BootSource, "BootSourceNamespace": c.BootSourceNamespace,
		"DefaultInstanceType": c.DefaultInstanceType, "DefaultPreference": c.DefaultPreference,
		"StorageClass": c.StorageClass, "GatewayID": c.GatewayID, "DiskSize": c.DiskSize,
	} {
		if strings.ContainsAny(value, "\x00\r\n") {
			errs = append(errs, fmt.Errorf("%s must not contain control characters", field))
		}
	}

	return errors.Join(errs...)
}
