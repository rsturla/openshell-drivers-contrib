package ec2

import (
	"math"
	"testing"

	"github.com/rsturla/openshell-drivers-contrib/gen/computepb"
)

func TestParseQuantities(t *testing.T) {
	for input, want := range map[string]float64{"4": 4, "500m": .5, "1.5": 1.5} {
		got, err := ParseCPUQuantity(input)
		if err != nil || math.Abs(got-want) > 1e-9 {
			t.Errorf("CPU %q=%v,%v want %v", input, got, err, want)
		}
	}
	for input, want := range map[string]float64{"1Gi": 1024, "4096Mi": 4096, "1G": 1e9 / (1024 * 1024), "1048576": 1} {
		got, err := ParseMemoryQuantity(input)
		if err != nil || math.Abs(got-want) > .01 {
			t.Errorf("memory %q=%v,%v want %v", input, got, err, want)
		}
	}
	for _, input := range []string{"", "-1", "NaN", "Inf", "-Inf", "bad"} {
		if _, err := ParseCPUQuantity(input); err == nil {
			t.Errorf("CPU accepted %q", input)
		}
		if _, err := ParseMemoryQuantity(input); err == nil {
			t.Errorf("memory accepted %q", input)
		}
	}
}

func TestSelectInstanceType(t *testing.T) {
	tests := []struct {
		name      string
		resources *computepb.DriverResourceRequirements
		want      string
		wantErr   bool
	}{
		{"default", nil, "c7i.4xlarge", false},
		{"request", &computepb.DriverResourceRequirements{CpuRequest: "4", MemoryRequest: "16Gi"}, "m7i.xlarge", false},
		{"limit controls sizing", &computepb.DriverResourceRequirements{CpuRequest: "1", CpuLimit: "8", MemoryLimit: "32Gi"}, "m7i.2xlarge", false},
		{"invalid", &computepb.DriverResourceRequirements{CpuRequest: "NaN"}, "", true},
		{"unsatisfiable", &computepb.DriverResourceRequirements{CpuRequest: "9999"}, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectInstanceType(tc.resources, "c7i.4xlarge")
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got %q,%v want %q err=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func FuzzParseCPUQuantity(f *testing.F) {
	for _, seed := range []string{"0", "1", "500m", "NaN", "Inf", "bad"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		value, err := ParseCPUQuantity(input)
		if err == nil && (value < 0 || math.IsNaN(value) || math.IsInf(value, 0)) {
			t.Fatalf("non-finite success for %q: %v", input, value)
		}
	})
}

func FuzzParseMemoryQuantity(f *testing.F) {
	for _, seed := range []string{"0", "1Gi", "500M", "NaN", "Inf", "bad"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		value, err := ParseMemoryQuantity(input)
		if err == nil && (value < 0 || math.IsNaN(value) || math.IsInf(value, 0)) {
			t.Fatalf("non-finite success for %q: %v", input, value)
		}
	})
}
