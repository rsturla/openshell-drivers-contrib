package kubevirt

import (
	"context"
	"time"
)

// VMProvider is the domain boundary consumed by the driver. KubeVirt API
// pagination, request construction, and response normalization stay behind it.
type VMProvider interface {
	CheckReady(ctx context.Context) error
	Launch(ctx context.Context, spec VMSpec) (VMInstance, error)
	List(ctx context.Context, filter VMFilter) ([]VMInstance, error)
	Stop(ctx context.Context, name string) error
	Terminate(ctx context.Context, name string) error
}

// VMSpec holds the parameters for launching a new KubeVirt VM.
type VMSpec struct {
	SandboxID, Name, SandboxNamespace, Workspace string
	GatewayID, InstanceType                      string
	BootSource, BootSourceNamespace              string
	Preference, StorageClass                     string
	CloudInit, ResourceKey, RequestDigest        string
	DiskSize                                     string
	StorageLabels                                map[string]string
	StorageAnnotations                           map[string]string
	CreatedAt                                    time.Time
	MaxLifetime                                  string
}

// VMFilter narrows the set of VMs returned by List.
type VMFilter struct {
	GatewayID string
	SandboxID string
	Name      string
}

// VMInstance joins an authoritative VirtualMachine with optional diagnostic
// state from its current VirtualMachineInstance.
type VMInstance struct {
	VMName    string // K8s object name
	SandboxID string
	Name      string // sandbox display name
	Namespace string
	Workspace string
	State     string // authoritative VM printable/driver state
	VMIPhase  string // optional VMI phase for diagnostics only
	IP        string
	CreatedAt time.Time
}
