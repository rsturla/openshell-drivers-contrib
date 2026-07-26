package ec2

import (
	"errors"
	"math"
	"sort"

	"github.com/rsturla/openshell-drivers-contrib/gen/computepb"
	"github.com/rsturla/openshell-drivers-contrib/pkg/quantity"
)

var ErrNoInstanceType = errors.New("no supported instance type satisfies the resource requirements")

// instanceSpec describes the vCPU and memory capacity of an EC2 instance type.
type instanceSpec struct {
	Name      string
	VCPUs     float64
	MemoryMiB float64
}

// instanceTypes is a built-in catalog of supported EC2 instance types.
// Covers c7i (compute), m7i (general purpose), and r7i (memory) families.
var instanceTypes = []instanceSpec{
	// c7i family — compute optimized
	{Name: "c7i.large", VCPUs: 2, MemoryMiB: 4096},
	{Name: "c7i.xlarge", VCPUs: 4, MemoryMiB: 8192},
	{Name: "c7i.2xlarge", VCPUs: 8, MemoryMiB: 16384},
	{Name: "c7i.4xlarge", VCPUs: 16, MemoryMiB: 32768},
	{Name: "c7i.8xlarge", VCPUs: 32, MemoryMiB: 65536},
	{Name: "c7i.12xlarge", VCPUs: 48, MemoryMiB: 98304},
	{Name: "c7i.16xlarge", VCPUs: 64, MemoryMiB: 131072},
	{Name: "c7i.24xlarge", VCPUs: 96, MemoryMiB: 196608},
	{Name: "c7i.48xlarge", VCPUs: 192, MemoryMiB: 393216},

	// m7i family — general purpose
	{Name: "m7i.large", VCPUs: 2, MemoryMiB: 8192},
	{Name: "m7i.xlarge", VCPUs: 4, MemoryMiB: 16384},
	{Name: "m7i.2xlarge", VCPUs: 8, MemoryMiB: 32768},
	{Name: "m7i.4xlarge", VCPUs: 16, MemoryMiB: 65536},
	{Name: "m7i.8xlarge", VCPUs: 32, MemoryMiB: 131072},
	{Name: "m7i.12xlarge", VCPUs: 48, MemoryMiB: 196608},
	{Name: "m7i.16xlarge", VCPUs: 64, MemoryMiB: 262144},
	{Name: "m7i.24xlarge", VCPUs: 96, MemoryMiB: 393216},
	{Name: "m7i.48xlarge", VCPUs: 192, MemoryMiB: 786432},

	// r7i family — memory optimized
	{Name: "r7i.large", VCPUs: 2, MemoryMiB: 16384},
	{Name: "r7i.xlarge", VCPUs: 4, MemoryMiB: 32768},
	{Name: "r7i.2xlarge", VCPUs: 8, MemoryMiB: 65536},
	{Name: "r7i.4xlarge", VCPUs: 16, MemoryMiB: 131072},
	{Name: "r7i.8xlarge", VCPUs: 32, MemoryMiB: 262144},
	{Name: "r7i.12xlarge", VCPUs: 48, MemoryMiB: 393216},
	{Name: "r7i.16xlarge", VCPUs: 64, MemoryMiB: 524288},
	{Name: "r7i.24xlarge", VCPUs: 96, MemoryMiB: 786432},
	{Name: "r7i.48xlarge", VCPUs: 192, MemoryMiB: 1572864},
}

// SelectInstanceType picks the smallest EC2 instance type from the built-in
// catalog that satisfies both requests and limits.
func SelectInstanceType(resources *computepb.DriverResourceRequirements, defaultType string) (string, error) {
	if resources == nil {
		return defaultType, nil
	}

	cpuReq := resources.GetCpuRequest()
	memReq := resources.GetMemoryRequest()

	if cpuReq == "" && resources.GetCpuLimit() == "" && memReq == "" && resources.GetMemoryLimit() == "" {
		return defaultType, nil
	}

	var cpuVCPUs float64
	var memMiB float64

	if cpuReq != "" {
		v, err := ParseCPUQuantity(cpuReq)
		if err != nil {
			return "", err
		}
		cpuVCPUs = v
	}
	if resources.GetCpuLimit() != "" {
		v, err := ParseCPUQuantity(resources.GetCpuLimit())
		if err != nil {
			return "", err
		}
		cpuVCPUs = math.Max(cpuVCPUs, v)
	}

	if memReq != "" {
		v, err := ParseMemoryQuantity(memReq)
		if err != nil {
			return "", err
		}
		memMiB = v
	}
	if resources.GetMemoryLimit() != "" {
		v, err := ParseMemoryQuantity(resources.GetMemoryLimit())
		if err != nil {
			return "", err
		}
		memMiB = math.Max(memMiB, v)
	}

	// Sort by total resource footprint (vCPUs + normalized memory) to pick smallest.
	sorted := make([]instanceSpec, len(instanceTypes))
	copy(sorted, instanceTypes)
	sort.Slice(sorted, func(i, j int) bool {
		// Primary sort by vCPUs, secondary by memory.
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

// ParseCPUQuantity delegates to quantity.ParseCPU for backward compatibility.
func ParseCPUQuantity(s string) (float64, error) { return quantity.ParseCPU(s) }

// ParseMemoryQuantity delegates to quantity.ParseMemory for backward compatibility.
func ParseMemoryQuantity(s string) (float64, error) { return quantity.ParseMemory(s) }
