package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	ec2driver "github.com/rsturla/openshell-drivers-contrib/drivers/ec2"
	"github.com/rsturla/openshell-drivers-contrib/pkg/bootstrap"
	"github.com/rsturla/openshell-drivers-contrib/pkg/server"
)

type appConfig struct {
	driver ec2driver.Config
	server server.Options
}

func loadConfig(args []string, getenv func(string) string) (appConfig, error) {
	defaults := ec2driver.DefaultConfig()
	fs := flag.NewFlagSet("openshell-driver-ec2", flag.ContinueOnError)
	socketPath := fs.String("socket", "/run/openshell/ec2.sock", "Unix socket path")
	bindAddress := fs.String("bind-address", "", "loopback TCP address for development")
	amiID := fs.String("ami-id", defaults.AMIID, "EC2 AMI ID")
	instanceType := fs.String("instance-type", defaults.DefaultInstance, "default EC2 instance type")
	subnetID := fs.String("subnet-id", defaults.SubnetID, "VPC subnet ID")
	securityGroupID := fs.String("security-group-id", defaults.SecurityGroupID, "security group ID")
	keyName := fs.String("key-name", defaults.KeyName, "optional EC2 key pair for debugging")
	gatewayEndpoint := fs.String("gateway-endpoint", defaults.Transport.GatewayEndpoint, "OpenShell gateway endpoint")
	gatewayCA := fs.String("gateway-ca-cert", "", "path to the gateway CA certificate")
	clientCert := fs.String("gateway-client-cert", "", "path to the supervisor client certificate")
	clientKey := fs.String("gateway-client-key", "", "path to the supervisor client private key")
	insecure := fs.Bool("insecure", false, "DANGER: allow plaintext supervisor-to-gateway transport")
	gatewayID := fs.String("gateway-id", defaults.GatewayID, "unique gateway identifier")
	region := fs.String("region", defaults.Region, "AWS region")
	pollInterval := fs.Duration("poll-interval", defaults.PollInterval, "EC2 poll interval")
	spot := fs.Bool("spot", defaults.UseSpot, "use spot instances")
	diskSizeGB := fs.Int("disk-size-gb", int(defaults.DiskSizeGB), "root EBS size in GB")
	maxInstances := fs.Int("max-instances", defaults.MaxInstances, "maximum concurrent sandbox instances")
	maxInstanceAge := fs.Duration("max-instance-age", defaults.MaxInstanceAge, "maximum instance lifetime")
	if err := fs.Parse(args); err != nil {
		return appConfig{}, err
	}
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var errs []error
	applyString := func(name, env string, target *string) {
		if !set[name] && getenv(env) != "" {
			*target = getenv(env)
		}
	}
	applyDuration := func(name, env string, target *time.Duration) {
		if set[name] || getenv(env) == "" {
			return
		}
		value, err := time.ParseDuration(getenv(env))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s=%q is not a duration: %w", env, getenv(env), err))
			return
		}
		*target = value
	}
	applyInt := func(name, env string, target *int) {
		if set[name] || getenv(env) == "" {
			return
		}
		value, err := strconv.Atoi(getenv(env))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s=%q is not an integer: %w", env, getenv(env), err))
			return
		}
		*target = value
	}
	applyBool := func(name, env string, target *bool) {
		if set[name] || getenv(env) == "" {
			return
		}
		value, err := strconv.ParseBool(getenv(env))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s=%q is not a boolean: %w", env, getenv(env), err))
			return
		}
		*target = value
	}
	applyString("socket", "OPENSHELL_DRIVER_SOCKET", socketPath)
	applyString("bind-address", "OPENSHELL_DRIVER_BIND", bindAddress)
	applyString("ami-id", "OPENSHELL_EC2_AMI_ID", amiID)
	applyString("instance-type", "OPENSHELL_EC2_INSTANCE_TYPE", instanceType)
	applyString("subnet-id", "OPENSHELL_EC2_SUBNET_ID", subnetID)
	applyString("security-group-id", "OPENSHELL_EC2_SECURITY_GROUP_ID", securityGroupID)
	applyString("key-name", "OPENSHELL_EC2_KEY_NAME", keyName)
	applyString("gateway-endpoint", "OPENSHELL_GATEWAY_ENDPOINT", gatewayEndpoint)
	applyString("gateway-ca-cert", "OPENSHELL_GATEWAY_CA_CERT", gatewayCA)
	applyString("gateway-client-cert", "OPENSHELL_GATEWAY_CLIENT_CERT", clientCert)
	applyString("gateway-client-key", "OPENSHELL_GATEWAY_CLIENT_KEY", clientKey)
	applyBool("insecure", "OPENSHELL_INSECURE", insecure)
	applyString("gateway-id", "OPENSHELL_GATEWAY_ID", gatewayID)
	applyString("region", "AWS_REGION", region)
	applyDuration("poll-interval", "OPENSHELL_EC2_POLL_INTERVAL", pollInterval)
	applyBool("spot", "OPENSHELL_EC2_SPOT", spot)
	applyInt("disk-size-gb", "OPENSHELL_EC2_DISK_SIZE_GB", diskSizeGB)
	applyInt("max-instances", "OPENSHELL_EC2_MAX_INSTANCES", maxInstances)
	applyDuration("max-instance-age", "OPENSHELL_EC2_MAX_INSTANCE_AGE", maxInstanceAge)
	if len(errs) > 0 {
		return appConfig{}, errors.Join(errs...)
	}
	transport, err := bootstrap.LoadTransportConfig(*gatewayEndpoint, *gatewayCA, *clientCert, *clientKey, *insecure)
	if err != nil {
		return appConfig{}, fmt.Errorf("transport configuration: %w", err)
	}
	cfg := ec2driver.Config{
		AMIID: *amiID, DefaultInstance: *instanceType, SubnetID: *subnetID, SecurityGroupID: *securityGroupID,
		KeyName: *keyName, Transport: transport, GatewayID: *gatewayID, Region: *region,
		PollInterval: *pollInterval, UseSpot: *spot, DiskSizeGB: int32(*diskSizeGB),
		MaxInstances: *maxInstances, MaxInstanceAge: *maxInstanceAge,
	}
	if err := cfg.Validate(); err != nil {
		return appConfig{}, err
	}
	return appConfig{driver: cfg, server: server.Options{SocketPath: *socketPath, BindAddress: *bindAddress, InsecureGatewayTransport: transport.Insecure}}, nil
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("%s %s\n", ec2driver.DriverName, ec2driver.DriverVersion)
		return
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	cfg, err := loadConfig(os.Args[1:], os.Getenv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	if cfg.driver.Transport.Insecure {
		slog.Warn("DANGER: supervisor-to-gateway transport is plaintext", "endpoint", cfg.driver.Transport.GatewayEndpoint)
	}
	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.driver.Region))
	if err != nil {
		slog.Error("failed to load AWS config", "region", cfg.driver.Region, "error", err)
		os.Exit(1)
	}
	drv, err := ec2driver.NewEC2Driver(cfg.driver, ec2driver.NewAWSEC2Client(awsCfg))
	if err != nil {
		slog.Error("failed to construct driver", "error", err)
		os.Exit(1)
	}
	slog.Info("starting openshell-driver-ec2", "driver", ec2driver.DriverName, "version", ec2driver.DriverVersion, "region", cfg.driver.Region, "instance_type", cfg.driver.DefaultInstance, "spot", cfg.driver.UseSpot, "max_instances", cfg.driver.MaxInstances, "max_instance_age", cfg.driver.MaxInstanceAge)
	if err := server.Run(ctx, cfg.server, drv); err != nil {
		slog.Error("driver exited with error", "error", err)
		os.Exit(1)
	}
}
