package ec2

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/rsturla/openshell-drivers-contrib/pkg/bootstrap"
)

var (
	amiPattern      = regexp.MustCompile(`^ami-[A-Za-z0-9]+$`)
	subnetPattern   = regexp.MustCompile(`^subnet-[A-Za-z0-9]+$`)
	securityPattern = regexp.MustCompile(`^sg-[A-Za-z0-9]+$`)
	regionPattern   = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z]+-[0-9]+$`)
	instancePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]+$`)
)

// Config holds the configuration for the EC2 compute driver.
type Config struct {
	AMIID           string
	DefaultInstance string // e.g. "c7i.4xlarge"
	SubnetID        string
	SecurityGroupID string
	KeyName         string // optional, for SSH debug
	Transport       bootstrap.TransportConfig
	GatewayID       string
	Region          string
	PollInterval    time.Duration
	UseSpot         bool
	DiskSizeGB      int32

	// Safeguards against orphaned instances
	MaxInstances   int           // cap on concurrent instances (default 10)
	MaxInstanceAge time.Duration // auto-terminate instances older than this (default 8h)
}

// DefaultConfig returns the operational defaults used by the EC2 command.
func DefaultConfig() Config {
	return Config{
		DefaultInstance: "c7i.4xlarge",
		Region:          "us-east-1",
		PollInterval:    15 * time.Second,
		DiskSizeGB:      200,
		MaxInstances:    10,
		MaxInstanceAge:  8 * time.Hour,
	}
}

// Validate checks that all required configuration fields are set.
func (c *Config) Validate() error {
	var errs []error

	if c.AMIID == "" {
		errs = append(errs, fmt.Errorf("amiID is required"))
	} else if !amiPattern.MatchString(c.AMIID) {
		errs = append(errs, fmt.Errorf("amiID must start with ami-"))
	}
	if c.SubnetID == "" {
		errs = append(errs, fmt.Errorf("subnetID is required"))
	} else if !subnetPattern.MatchString(c.SubnetID) {
		errs = append(errs, fmt.Errorf("subnetID must start with subnet-"))
	}
	if c.SecurityGroupID == "" {
		errs = append(errs, fmt.Errorf("securityGroupID is required"))
	} else if !securityPattern.MatchString(c.SecurityGroupID) {
		errs = append(errs, fmt.Errorf("securityGroupID must start with sg-"))
	}
	if err := c.Transport.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("transport: %w", err))
	}
	if c.DefaultInstance == "" {
		errs = append(errs, fmt.Errorf("defaultInstance is required"))
	} else if !instancePattern.MatchString(c.DefaultInstance) {
		errs = append(errs, fmt.Errorf("defaultInstance contains invalid characters"))
	}
	if c.GatewayID == "" {
		errs = append(errs, fmt.Errorf("gatewayID is required"))
	} else if len(c.GatewayID) > 256 {
		errs = append(errs, fmt.Errorf("gatewayID must not exceed 256 bytes"))
	}
	if c.Region == "" {
		errs = append(errs, fmt.Errorf("region is required"))
	} else if !regionPattern.MatchString(c.Region) {
		errs = append(errs, fmt.Errorf("region has invalid format"))
	}
	if c.PollInterval <= 0 || c.PollInterval > time.Hour {
		errs = append(errs, fmt.Errorf("pollInterval must be greater than zero and at most 1h"))
	}
	if c.DiskSizeGB <= 0 || c.DiskSizeGB > 16384 {
		errs = append(errs, fmt.Errorf("diskSizeGB must be between 1 and 16384"))
	}
	if c.MaxInstances <= 0 || c.MaxInstances > 1000 {
		errs = append(errs, fmt.Errorf("maxInstances must be between 1 and 1000"))
	}
	if c.MaxInstanceAge <= 0 || c.MaxInstanceAge > 30*24*time.Hour {
		errs = append(errs, fmt.Errorf("maxInstanceAge must be greater than zero and at most 720h"))
	}
	for field, value := range map[string]string{
		"AMIID": c.AMIID, "SubnetID": c.SubnetID, "SecurityGroupID": c.SecurityGroupID,
		"GatewayID": c.GatewayID, "DefaultInstance": c.DefaultInstance, "Region": c.Region, "KeyName": c.KeyName,
	} {
		if strings.ContainsAny(value, "\x00\r\n") {
			errs = append(errs, fmt.Errorf("%s must not contain control characters", field))
		}
	}

	return errors.Join(errs...)
}
