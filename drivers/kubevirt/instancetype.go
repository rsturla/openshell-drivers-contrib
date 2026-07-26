package kubevirt

import (
	"errors"
	"math"
	"sort"

	"github.com/rsturla/openshell-drivers-contrib/gen/computepb"
	"github.com/rsturla/openshell-drivers-contrib/pkg/quantity"
)

var ErrNoInstanceType = errors.New("no supported instance type satisfies the resource requirements")

// instanceSpec describes the vCPU and memory capacity of a KubeVirt cluster
// instance type.
type instanceSpec struct {
	Name      string
	VCPUs     float64
	MemoryMiB float64
}

// instanceTypes is a built-in catalog of KubeVirt cluster instance types
// available on OpenShift clusters. Covers cx1 (compute) and d1 (general).
var instanceTypes = []instanceSpec{
	// cx1 family -- compute optimized (2:1 memory-to-CPU ratio in GiB)
	{Name: "cx1.medium", VCPUs: 1, MemoryMiB: 2 * 1024},
	{Name: "cx1.large", VCPUs: 2, MemoryMiB: 4 * 1024},
	{Name: "cx1.xlarge", VCPUs: 4, MemoryMiB: 8 * 1024},
	{Name: "cx1.2xlarge", VCPUs: 8, MemoryMiB: 16 * 1024},
	{Name: "cx1.4xlarge", VCPUs: 16, MemoryMiB: 32 * 1024},
	{Name: "cx1.8xlarge", VCPUs: 32, MemoryMiB: 64 * 1024},

	// d1 family -- general purpose (4:1 memory-to-CPU ratio in GiB)
	{Name: "d1.micro", VCPUs: 1, MemoryMiB: 1 * 1024},
	{Name: "d1.small", VCPUs: 1, MemoryMiB: 2 * 1024},
	{Name: "d1.medium", VCPUs: 1, MemoryMiB: 4 * 1024},
	{Name: "d1.large", VCPUs: 2, MemoryMiB: 8 * 1024},
	{Name: "d1.xlarge", VCPUs: 4, MemoryMiB: 16 * 1024},
	{Name: "d1.2xlarge", VCPUs: 8, MemoryMiB: 32 * 1024},
	{Name: "d1.4xlarge", VCPUs: 16, MemoryMiB: 64 * 1024},
	{Name: "d1.8xlarge", VCPUs: 32, MemoryMiB: 128 * 1024},
}

// SelectInstanceType picks the smallest KubeVirt cluster instance type from
// the built-in catalog that satisfies both requests and limits.
func SelectInstanceType(resources *computepb.DriverResourceRequirements, defaultType string) (string, error) {
	if resources == nil {
		if defaultType == "" {
			return "", ErrNoInstanceType
		}
		return defaultType, nil
	}

	cpuReq := resources.GetCpuRequest()
	memReq := resources.GetMemoryRequest()

	if cpuReq == "" && resources.GetCpuLimit() == "" && memReq == "" && resources.GetMemoryLimit() == "" {
		if defaultType == "" {
			return "", ErrNoInstanceType
		}
		return defaultType, nil
	}

	var cpuVCPUs float64
	var memMiB float64

	if cpuReq != "" {
		v, err := quantity.ParseCPU(cpuReq)
		if err != nil {
			return "", err
		}
		cpuVCPUs = v
	}
	if resources.GetCpuLimit() != "" {
		v, err := quantity.ParseCPU(resources.GetCpuLimit())
		if err != nil {
			return "", err
		}
		cpuVCPUs = math.Max(cpuVCPUs, v)
	}

	if memReq != "" {
		v, err := quantity.ParseMemory(memReq)
		if err != nil {
			return "", err
		}
		memMiB = v
	}
	if resources.GetMemoryLimit() != "" {
		v, err := quantity.ParseMemory(resources.GetMemoryLimit())
		if err != nil {
			return "", err
		}
		memMiB = math.Max(memMiB, v)
	}

	// Sort by total resource footprint (vCPUs + normalized memory) to pick smallest.
	sorted := make([]instanceSpec, len(instanceTypes))
	copy(sorted, instanceTypes)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].VCPUs != sorted[j].VCPUs {
			return sorted[i].VCPUs < sorted[j].VCPUs
		}
		return sorted[i].MemoryMiB < sorted[j].MemoryMiB
	})

	for _, inst := range sorted {
		if inst.VCPUs >= cpuVCPUs && inst.MemoryMiB >= memMiB {
			return inst.Name, nil
		}
	}

	return "", ErrNoInstanceType
}
