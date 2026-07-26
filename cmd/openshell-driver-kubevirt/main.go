package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	kubevirtdriver "github.com/rsturla/openshell-drivers-contrib/drivers/kubevirt"
	"github.com/rsturla/openshell-drivers-contrib/pkg/bootstrap"
	"github.com/rsturla/openshell-drivers-contrib/pkg/server"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type appConfig struct {
	driver     kubevirtdriver.Config
	server     server.Options
	kubeconfig string
}

func loadConfig(args []string, getenv func(string) string) (appConfig, error) {
	defaults := kubevirtdriver.DefaultConfig()
	fs := flag.NewFlagSet("openshell-driver-kubevirt", flag.ContinueOnError)
	socketPath := fs.String("socket", "/run/openshell/kubevirt.sock", "Unix socket path")
	bindAddress := fs.String("bind-address", "", "loopback TCP address for development")
	namespace := fs.String("namespace", defaults.Namespace, "Kubernetes namespace for VMs")
	bootSource := fs.String("boot-source", defaults.BootSource, "DataSource name for VM boot volume")
	bootSourceNamespace := fs.String("boot-source-namespace", defaults.BootSourceNamespace, "namespace of the boot source")
	defaultInstanceType := fs.String("default-instancetype", defaults.DefaultInstanceType, "default VirtualMachineInstancetype name")
	defaultPreference := fs.String("default-preference", defaults.DefaultPreference, "optional VirtualMachineClusterPreference name")
	storageClass := fs.String("storage-class", defaults.StorageClass, "encrypted StorageClass for sandbox disks")
	gatewayEndpoint := fs.String("gateway-endpoint", defaults.Transport.GatewayEndpoint, "OpenShell gateway endpoint")
	gatewayCA := fs.String("gateway-ca-cert", "", "path to the gateway CA certificate")
	clientCert := fs.String("gateway-client-cert", "", "path to the supervisor client certificate")
	clientKey := fs.String("gateway-client-key", "", "path to the supervisor client private key")
	insecure := fs.Bool("insecure", false, "DANGER: allow plaintext supervisor-to-gateway transport")
	gatewayID := fs.String("gateway-id", defaults.GatewayID, "unique gateway identifier")
	diskSize := fs.String("disk-size", defaults.DiskSize, "root disk size (e.g. 100Gi)")
	pollInterval := fs.Duration("poll-interval", defaults.PollInterval, "reconciliation poll interval")
	maxInstances := fs.Int("max-instances", defaults.MaxInstances, "maximum concurrent VM instances")
	maxInstanceAge := fs.Duration("max-instance-age", defaults.MaxInstanceAge, "maximum VM lifetime")
	storageLabel := fs.String("storage-label", "", "extra label for PVCs (key=value, e.g. paas.redhat.com/appcode=MYAPP)")
	storageAnnotation := fs.String("storage-annotation", "", "extra annotation for PVCs (key=value, e.g. kubernetes.io/reclaimPolicy=Delete)")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (optional, for out-of-cluster development)")
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
	applyString("namespace", "OPENSHELL_KUBEVIRT_NAMESPACE", namespace)
	applyString("boot-source", "OPENSHELL_KUBEVIRT_BOOT_SOURCE", bootSource)
	applyString("boot-source-namespace", "OPENSHELL_KUBEVIRT_BOOT_SOURCE_NAMESPACE", bootSourceNamespace)
	applyString("default-instancetype", "OPENSHELL_KUBEVIRT_DEFAULT_INSTANCETYPE", defaultInstanceType)
	applyString("default-preference", "OPENSHELL_KUBEVIRT_DEFAULT_PREFERENCE", defaultPreference)
	applyString("storage-class", "OPENSHELL_KUBEVIRT_STORAGE_CLASS", storageClass)
	applyString("gateway-endpoint", "OPENSHELL_GATEWAY_ENDPOINT", gatewayEndpoint)
	applyString("gateway-ca-cert", "OPENSHELL_GATEWAY_CA_CERT", gatewayCA)
	applyString("gateway-client-cert", "OPENSHELL_GATEWAY_CLIENT_CERT", clientCert)
	applyString("gateway-client-key", "OPENSHELL_GATEWAY_CLIENT_KEY", clientKey)
	applyBool("insecure", "OPENSHELL_INSECURE", insecure)
	applyString("gateway-id", "OPENSHELL_GATEWAY_ID", gatewayID)
	applyString("disk-size", "OPENSHELL_KUBEVIRT_DISK_SIZE", diskSize)
	applyString("kubeconfig", "KUBECONFIG", kubeconfig)
	applyDuration("poll-interval", "OPENSHELL_KUBEVIRT_POLL_INTERVAL", pollInterval)
	applyInt("max-instances", "OPENSHELL_KUBEVIRT_MAX_INSTANCES", maxInstances)
	applyDuration("max-instance-age", "OPENSHELL_KUBEVIRT_MAX_INSTANCE_AGE", maxInstanceAge)
	if len(errs) > 0 {
		return appConfig{}, errors.Join(errs...)
	}
	transport, err := bootstrap.LoadTransportConfig(*gatewayEndpoint, *gatewayCA, *clientCert, *clientKey, *insecure)
	if err != nil {
		return appConfig{}, fmt.Errorf("transport configuration: %w", err)
	}
	storageLabels := make(map[string]string)
	if *storageLabel != "" {
		if k, v, ok := strings.Cut(*storageLabel, "="); ok {
			storageLabels[k] = v
		} else {
			return appConfig{}, fmt.Errorf("--storage-label must be key=value, got %q", *storageLabel)
		}
	}
	storageAnns := make(map[string]string)
	if *storageAnnotation != "" {
		if k, v, ok := strings.Cut(*storageAnnotation, "="); ok {
			storageAnns[k] = v
		} else {
			return appConfig{}, fmt.Errorf("--storage-annotation must be key=value, got %q", *storageAnnotation)
		}
	}
	cfg := kubevirtdriver.Config{
		Namespace: *namespace, BootSource: *bootSource, BootSourceNamespace: *bootSourceNamespace,
		DefaultInstanceType: *defaultInstanceType, DefaultPreference: *defaultPreference, StorageClass: *storageClass,
		StorageLabels: storageLabels, StorageAnnotations: storageAnns,
		Transport: transport, GatewayID: *gatewayID,
		DiskSize: *diskSize, PollInterval: *pollInterval, MaxInstances: *maxInstances, MaxInstanceAge: *maxInstanceAge,
	}
	if err := cfg.Validate(); err != nil {
		return appConfig{}, err
	}
	return appConfig{
		driver:     cfg,
		server:     server.Options{SocketPath: *socketPath, BindAddress: *bindAddress, InsecureGatewayTransport: transport.Insecure},
		kubeconfig: *kubeconfig,
	}, nil
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("%s %s\n", kubevirtdriver.DriverName, kubevirtdriver.DriverVersion)
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

	var restConfig *rest.Config
	if cfg.kubeconfig != "" {
		restConfig, err = clientcmd.BuildConfigFromFlags("", cfg.kubeconfig)
	} else {
		restConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		slog.Error("failed to load Kubernetes client configuration", "error", err)
		os.Exit(1)
	}
	provider, err := kubevirtdriver.NewKubeAPIProvider(restConfig, cfg.driver)
	if err != nil {
		slog.Error("failed to construct KubeVirt provider", "error", err)
		os.Exit(1)
	}

	drv, err := kubevirtdriver.NewKubeVirtDriver(cfg.driver, provider)
	if err != nil {
		slog.Error("failed to construct driver", "error", err)
		os.Exit(1)
	}
	slog.Info("starting openshell-driver-kubevirt",
		"driver", kubevirtdriver.DriverName,
		"version", kubevirtdriver.DriverVersion,
		"namespace", cfg.driver.Namespace,
		"instance_type", cfg.driver.DefaultInstanceType,
		"preference", cfg.driver.DefaultPreference,
		"storage_class", cfg.driver.StorageClass,
		"max_instances", cfg.driver.MaxInstances,
		"max_instance_age", cfg.driver.MaxInstanceAge,
	)
	if err := server.Run(ctx, cfg.server, drv); err != nil {
		slog.Error("driver exited with error", "error", err)
		os.Exit(1)
	}
}
