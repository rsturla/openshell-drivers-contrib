package quantity

import (
	"math"
	"testing"
)

func TestParseCPU(t *testing.T) {
	tests := map[string]float64{
		"4":     4,
		"500m":  0.5,
		"1.5":   1.5,
		"1500m": 1.5,
		"0":     0,
		"100m":  0.1,
	}
	for input, want := range tests {
		got, err := ParseCPU(input)
		if err != nil || math.Abs(got-want) > 1e-9 {
			t.Errorf("ParseCPU(%q) = %v, %v; want %v", input, got, err, want)
		}
	}
}

func TestParseCPUErrors(t *testing.T) {
	for _, input := range []string{"", "-1", "NaN", "Inf", "-Inf", "bad", "-500m"} {
		if _, err := ParseCPU(input); err == nil {
			t.Errorf("ParseCPU(%q) should have returned error", input)
		}
	}
}

func TestParseMemory(t *testing.T) {
	tests := map[string]float64{
		"1Gi":     1024,
		"4096Mi":  4096,
		"1G":      1e9 / (1024 * 1024),
		"500M":    500e6 / (1024 * 1024),
		"1Ti":     1048576,
		"1048576": 1, // 1 MiB in bytes
		"1Ki":     1.0 / 1024,
		"0":       0,
	}
	for input, want := range tests {
		got, err := ParseMemory(input)
		if err != nil || math.Abs(got-want) > 0.01 {
			t.Errorf("ParseMemory(%q) = %v, %v; want %v", input, got, err, want)
		}
	}
}

func TestParseMemoryErrors(t *testing.T) {
	for _, input := range []string{"", "-1", "NaN", "Inf", "-Inf", "bad", "-1Gi"} {
		if _, err := ParseMemory(input); err == nil {
			t.Errorf("ParseMemory(%q) should have returned error", input)
		}
	}
}

func TestValidateRequirements(t *testing.T) {
	if err := ValidateRequirements("500m", "2", "1Gi", "2Gi"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRequirements("NaN", "bad", "-1Gi", "garbage"); err == nil {
		t.Fatal("expected joined validation errors")
	}
}

func FuzzParseCPU(f *testing.F) {
	for _, seed := range []string{"0", "1", "500m", "NaN", "Inf", "bad"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		value, err := ParseCPU(input)
		if err == nil && (value < 0 || math.IsNaN(value) || math.IsInf(value, 0)) {
			t.Fatalf("non-finite success for %q: %v", input, value)
		}
	})
}

func FuzzParseMemory(f *testing.F) {
	for _, seed := range []string{"0", "1Gi", "500M", "NaN", "Inf", "bad"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		value, err := ParseMemory(input)
		if err == nil && (value < 0 || math.IsNaN(value) || math.IsInf(value, 0)) {
			t.Fatalf("non-finite success for %q: %v", input, value)
		}
	})
}
