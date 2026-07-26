package ec2

import (
	"strings"
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	valid := testConfig()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	tests := map[string]func(*Config){
		"AMI":               func(c *Config) { c.AMIID = "" },
		"subnet":            func(c *Config) { c.SubnetID = "" },
		"security group":    func(c *Config) { c.SecurityGroupID = "" },
		"gateway URL":       func(c *Config) { c.Transport.GatewayEndpoint = "javascript:bad" },
		"gateway ID":        func(c *Config) { c.GatewayID = "" },
		"region":            func(c *Config) { c.Region = "" },
		"instance":          func(c *Config) { c.DefaultInstance = "" },
		"zero poll":         func(c *Config) { c.PollInterval = 0 },
		"negative poll":     func(c *Config) { c.PollInterval = -time.Second },
		"disk":              func(c *Config) { c.DiskSizeGB = 0 },
		"capacity":          func(c *Config) { c.MaxInstances = 0 },
		"lifetime":          func(c *Config) { c.MaxInstanceAge = 0 },
		"control character": func(c *Config) { c.GatewayID = "bad\nvalue" },
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
	for _, field := range []string{"amiID", "subnetID", "securityGroupID", "transport", "pollInterval", "diskSizeGB"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("missing %s in %v", field, err)
		}
	}
}
