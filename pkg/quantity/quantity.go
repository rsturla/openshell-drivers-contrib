// Package quantity provides Kubernetes-style resource quantity parsing for
// OpenShell compute drivers.
package quantity

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ValidateRequirements validates optional CPU and memory request/limit values.
func ValidateRequirements(cpuRequest, cpuLimit, memoryRequest, memoryLimit string) error {
	var errs []error
	for field, value := range map[string]string{"cpu request": cpuRequest, "cpu limit": cpuLimit} {
		if value != "" {
			if _, err := ParseCPU(value); err != nil {
				errs = append(errs, fmt.Errorf("invalid %s: %w", field, err))
			}
		}
	}
	for field, value := range map[string]string{"memory request": memoryRequest, "memory limit": memoryLimit} {
		if value != "" {
			if _, err := ParseMemory(value); err != nil {
				errs = append(errs, fmt.Errorf("invalid %s: %w", field, err))
			}
		}
	}
	return errors.Join(errs...)
}

// ParseCPU parses a Kubernetes-style CPU quantity string and returns the
// value in vCPUs (cores). Examples:
//
//	"4"     -> 4.0
//	"500m"  -> 0.5
//	"4.0"   -> 4.0
//	"1500m" -> 1.5
func ParseCPU(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty CPU quantity")
	}

	if strings.HasSuffix(s, "m") {
		millis, err := strconv.ParseFloat(strings.TrimSuffix(s, "m"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid CPU millicore value %q: %w", s, err)
		}
		if millis < 0 || math.IsNaN(millis) || math.IsInf(millis, 0) {
			return 0, fmt.Errorf("negative CPU quantity %q", s)
		}
		return millis / 1000.0, nil
	}

	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid CPU quantity %q: %w", s, err)
	}
	if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("negative CPU quantity %q", s)
	}
	return v, nil
}

// ParseMemory parses a Kubernetes-style memory quantity string and returns
// the value in MiB. Examples:
//
//	"32Gi"   -> 32768.0  (32 * 1024)
//	"4096Mi" -> 4096.0
//	"1G"     -> 953.674... (1000^3 / 1024^2)
//	"500M"   -> 476.837... (500 * 1000^2 / 1024^2)
//	"1Ti"    -> 1048576.0
//
// Supported suffixes:
//
//	Binary: Ki, Mi, Gi, Ti, Pi, Ei
//	Decimal: K (k), M, G, T, P, E
//	None: plain bytes
func ParseMemory(s string) (float64, error) {
	const bytesPerMiB = 1024 * 1024

	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty memory quantity")
	}

	// Binary suffixes (IEC): Ki, Mi, Gi, Ti, Pi, Ei
	type suffix struct {
		text       string
		multiplier float64
	}

	binarySuffixes := []suffix{
		{"Ei", bytesPerMiB * bytesPerMiB}, // EiB -> MiB
		{"Pi", 1024 * bytesPerMiB},        // PiB -> MiB
		{"Ti", bytesPerMiB},               // TiB -> MiB
		{"Gi", 1024},                      // GiB -> MiB
		{"Mi", 1},                         // MiB -> MiB
		{"Ki", 1.0 / 1024},                // KiB -> MiB
	}

	decimalSuffixes := []suffix{
		{"E", 1e18 / bytesPerMiB},
		{"P", 1e15 / bytesPerMiB},
		{"T", 1e12 / bytesPerMiB},
		{"G", 1e9 / bytesPerMiB},
		{"M", 1e6 / bytesPerMiB},
		{"K", 1e3 / bytesPerMiB},
		{"k", 1e3 / bytesPerMiB},
	}

	// Try binary suffixes first (longer, so they won't conflict).
	for _, suf := range binarySuffixes {
		if strings.HasSuffix(s, suf.text) {
			numStr := strings.TrimSuffix(s, suf.text)
			v, err := strconv.ParseFloat(numStr, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid memory quantity %q: %w", s, err)
			}
			if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				return 0, fmt.Errorf("negative memory quantity %q", s)
			}
			return v * suf.multiplier, nil
		}
	}

	// Try decimal suffixes.
	for _, suf := range decimalSuffixes {
		if strings.HasSuffix(s, suf.text) {
			numStr := strings.TrimSuffix(s, suf.text)
			v, err := strconv.ParseFloat(numStr, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid memory quantity %q: %w", s, err)
			}
			if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				return 0, fmt.Errorf("negative memory quantity %q", s)
			}
			return v * suf.multiplier, nil
		}
	}

	// No suffix: plain bytes.
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory quantity %q: %w", s, err)
	}
	if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("negative memory quantity %q", s)
	}
	return v / bytesPerMiB, nil // bytes -> MiB
}
