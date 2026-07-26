package kubevirt

import (
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{
		Namespace:           "openshell",
		BootSource:          "fedora",
		BootSourceNamespace: "openshift-virtualization-os-images",
		DefaultInstanceType: "cx1.4xlarge",
		StorageClass:        "encrypted-rbd",
		Transport:           insecureTransport("http://gateway.example"),
		GatewayID:           "gw-test",
		DiskSize:            "100Gi",
		PollInterval:        time.Second,
		MaxInstances:        10,
		MaxInstanceAge:      8 * time.Hour,
	}
}

func TestConfigValidate(t *testing.T) {
	valid := testConfig()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	tests := map[string]func(*Config){
		"namespace empty":        func(c *Config) { c.Namespace = "" },
		"namespace invalid":      func(c *Config) { c.Namespace = "INVALID" },
		"namespace too long":     func(c *Config) { c.Namespace = strings.Repeat("a", 64) },
		"boot source":            func(c *Config) { c.BootSource = "" },
		"boot source invalid":    func(c *Config) { c.BootSource = "BAD/SOURCE" },
		"boot source ns":         func(c *Config) { c.BootSourceNamespace = "" },
		"boot source ns invalid": func(c *Config) { c.BootSourceNamespace = "BAD_NS" },
		"instance type empty":    func(c *Config) { c.DefaultInstanceType = "" },
		"instance type invalid":  func(c *Config) { c.DefaultInstanceType = "BAD TYPE!" },
		"storage class empty":    func(c *Config) { c.StorageClass = "" },
		"storage class invalid":  func(c *Config) { c.StorageClass = "BAD CLASS" },
		"gateway URL":            func(c *Config) { c.Transport.GatewayEndpoint = "javascript:bad" },
		"gateway URL empty":      func(c *Config) { c.Transport.GatewayEndpoint = "" },
		"gateway ID":             func(c *Config) { c.GatewayID = "" },
		"gateway ID too long":    func(c *Config) { c.GatewayID = strings.Repeat("x", 257) },
		"disk size empty":        func(c *Config) { c.DiskSize = "" },
		"disk size invalid":      func(c *Config) { c.DiskSize = "not-a-quantity" },
		"disk size zero":         func(c *Config) { c.DiskSize = "0" },
		"disk size negative":     func(c *Config) { c.DiskSize = "-1Gi" },
		"disk size huge":         func(c *Config) { c.DiskSize = "17Ti" },
		"zero poll":              func(c *Config) { c.PollInterval = 0 },
		"negative poll":          func(c *Config) { c.PollInterval = -time.Second },
		"poll too large":         func(c *Config) { c.PollInterval = 2 * time.Hour },
		"capacity zero":          func(c *Config) { c.MaxInstances = 0 },
		"capacity too large":     func(c *Config) { c.MaxInstances = 1001 },
		"lifetime zero":          func(c *Config) { c.MaxInstanceAge = 0 },
		"lifetime too large":     func(c *Config) { c.MaxInstanceAge = 31 * 24 * time.Hour },
		"control character":      func(c *Config) { c.GatewayID = "bad\nvalue" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestConfigReportsAllErrors(t *testing.T) {
	err := (&Config{}).Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, field := range []string{"namespace", "bootSource", "bootSourceNamespace", "defaultInstanceType", "storageClass", "transport", "gatewayID", "diskSize", "pollInterval", "maxInstances", "maxInstanceAge"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("missing %s in %v", field, err)
		}
	}
}

func TestDefaultConfigIsValid(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Transport = insecureTransport("http://gateway.example")
	cfg.GatewayID = "gw-test"
	cfg.StorageClass = "encrypted-rbd"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig with required fields should be valid: %v", err)
	}
}
