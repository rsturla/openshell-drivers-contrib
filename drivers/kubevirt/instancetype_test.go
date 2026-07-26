package kubevirt

import (
	"math"
	"testing"

	"github.com/rsturla/openshell-drivers-contrib/gen/computepb"
	"github.com/rsturla/openshell-drivers-contrib/pkg/quantity"
)

func TestSelectInstanceType(t *testing.T) {
	tests := []struct {
		name      string
		resources *computepb.DriverResourceRequirements
		want      string
		wantErr   bool
	}{
		{"nil resources returns default", nil, "cx1.4xlarge", false},
		{"empty resources returns default", &computepb.DriverResourceRequirements{}, "cx1.4xlarge", false},
		{"small cpu request", &computepb.DriverResourceRequirements{CpuRequest: "1", MemoryRequest: "1Gi"}, "d1.micro", false},
		{"medium request picks cx1", &computepb.DriverResourceRequirements{CpuRequest: "4", MemoryRequest: "8Gi"}, "cx1.xlarge", false},
		{"limit controls sizing", &computepb.DriverResourceRequirements{CpuRequest: "1", CpuLimit: "8", MemoryLimit: "32Gi"}, "d1.2xlarge", false},
		{"high memory picks d1", &computepb.DriverResourceRequirements{CpuRequest: "2", MemoryRequest: "8Gi"}, "d1.large", false},
		{"invalid cpu", &computepb.DriverResourceRequirements{CpuRequest: "NaN"}, "", true},
		{"invalid memory", &computepb.DriverResourceRequirements{MemoryRequest: "garbage"}, "", true},
		{"unsatisfiable", &computepb.DriverResourceRequirements{CpuRequest: "9999"}, "", true},
		{"empty default with nil resources", nil, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defaultType := "cx1.4xlarge"
			if tc.name == "empty default with nil resources" {
				defaultType = ""
			}
			got, err := SelectInstanceType(tc.resources, defaultType)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got %q,%v want %q err=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestSelectInstanceTypePrefersSmallerFootprint(t *testing.T) {
	// 1 vCPU, 2Gi should pick cx1.medium (1/2Gi) over d1.large (2/8Gi)
	result, err := SelectInstanceType(&computepb.DriverResourceRequirements{
		CpuRequest: "1", MemoryRequest: "2Gi",
	}, "cx1.4xlarge")
	if err != nil {
		t.Fatal(err)
	}
	if result != "cx1.medium" {
		t.Fatalf("expected cx1.medium, got %s", result)
	}
}

func TestSelectInstanceTypeMemoryOnly(t *testing.T) {
	// Memory-only request: 64Gi, no CPU requirement
	result, err := SelectInstanceType(&computepb.DriverResourceRequirements{
		MemoryRequest: "64Gi",
	}, "cx1.4xlarge")
	if err != nil {
		t.Fatal(err)
	}
	// cx1.8xlarge has 64Gi with 32 vCPUs; d1.4xlarge has 64Gi with 16 vCPUs
	// d1.4xlarge sorts first (fewer vCPUs)
	if result != "d1.4xlarge" {
		t.Fatalf("expected d1.4xlarge for 64Gi memory-only, got %s", result)
	}
}

func TestParseQuantitiesViaSharedPackage(t *testing.T) {
	// Verify the shared quantity package works correctly for some key values.
	for input, want := range map[string]float64{"4": 4, "500m": .5, "1.5": 1.5} {
		got, err := quantity.ParseCPU(input)
		if err != nil || math.Abs(got-want) > 1e-9 {
			t.Errorf("CPU %q=%v,%v want %v", input, got, err, want)
		}
	}
	for input, want := range map[string]float64{"1Gi": 1024, "4096Mi": 4096, "1G": 1e9 / (1024 * 1024)} {
		got, err := quantity.ParseMemory(input)
		if err != nil || math.Abs(got-want) > .01 {
			t.Errorf("memory %q=%v,%v want %v", input, got, err, want)
		}
	}
	for _, input := range []string{"", "-1", "NaN", "Inf", "-Inf", "bad"} {
		if _, err := quantity.ParseCPU(input); err == nil {
			t.Errorf("CPU accepted %q", input)
		}
		if _, err := quantity.ParseMemory(input); err == nil {
			t.Errorf("memory accepted %q", input)
		}
	}
}

func FuzzSelectInstanceType(f *testing.F) {
	for _, seed := range []string{"0", "1", "500m", "16", "100Gi", "NaN", "bad"} {
		f.Add(seed, seed)
	}
	f.Fuzz(func(t *testing.T, cpu, mem string) {
		// Should never panic.
		_, _ = SelectInstanceType(&computepb.DriverResourceRequirements{
			CpuRequest: cpu, MemoryRequest: mem,
		}, "cx1.4xlarge")
	})
}
