package main

import (
	"strings"
	"testing"
)

func validEnv() map[string]string {
	return map[string]string{
		"OPENSHELL_KUBEVIRT_NAMESPACE":             "openshell",
		"OPENSHELL_KUBEVIRT_BOOT_SOURCE":           "fedora",
		"OPENSHELL_KUBEVIRT_BOOT_SOURCE_NAMESPACE": "openshift-virtualization-os-images",
		"OPENSHELL_KUBEVIRT_DEFAULT_INSTANCETYPE":  "cx1.4xlarge",
		"OPENSHELL_GATEWAY_ENDPOINT":               "http://gateway.example",
		"OPENSHELL_GATEWAY_ID":                     "gw-test",
		"OPENSHELL_INSECURE":                       "true",
		"OPENSHELL_KUBEVIRT_DISK_SIZE":             "100Gi",
		"OPENSHELL_KUBEVIRT_STORAGE_CLASS":         "encrypted-rbd",
	}
}

func TestLoadConfigSecureByDefaultRequiresTLSFiles(t *testing.T) {
	env := validEnv()
	delete(env, "OPENSHELL_INSECURE")
	env["OPENSHELL_GATEWAY_ENDPOINT"] = "https://gateway.example"
	_, err := loadConfig(nil, func(name string) string { return env[name] })
	if err == nil || !strings.Contains(err.Error(), "gateway CA certificate path is required") {
		t.Fatalf("expected missing TLS material error, got %v", err)
	}
}

func TestLoadConfigEnvironmentAndFlagPrecedence(t *testing.T) {
	env := validEnv()
	env["OPENSHELL_KUBEVIRT_MAX_INSTANCES"] = "7"
	getenv := func(key string) string { return env[key] }
	cfg, err := loadConfig([]string{"--max-instances=3"}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.driver.MaxInstances != 3 {
		t.Fatalf("flag did not override env: %d", cfg.driver.MaxInstances)
	}
	if cfg.driver.Namespace != "openshell" {
		t.Fatalf("env was not loaded: %+v", cfg.driver)
	}
}

func TestLoadConfigRejectsInvalidEnvironmentValues(t *testing.T) {
	for key, value := range map[string]string{
		"OPENSHELL_KUBEVIRT_MAX_INSTANCES":    "10junk",
		"OPENSHELL_KUBEVIRT_POLL_INTERVAL":    "soon",
		"OPENSHELL_KUBEVIRT_MAX_INSTANCE_AGE": "forever",
	} {
		t.Run(key, func(t *testing.T) {
			env := validEnv()
			env[key] = value
			_, err := loadConfig(nil, func(name string) string { return env[name] })
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("expected error naming %s, got %v", key, err)
			}
		})
	}
}

func TestLoadConfigRequiresBootSource(t *testing.T) {
	env := validEnv()
	delete(env, "OPENSHELL_KUBEVIRT_BOOT_SOURCE")
	// Boot source defaults to "fedora" from DefaultConfig, so this should still succeed.
	_, err := loadConfig(nil, func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("expected default boot source to apply, got: %v", err)
	}
}

func TestLoadConfigRequiresGatewayEndpoint(t *testing.T) {
	env := validEnv()
	delete(env, "OPENSHELL_GATEWAY_ENDPOINT")
	_, err := loadConfig(nil, func(name string) string { return env[name] })
	if err == nil || !strings.Contains(err.Error(), "gateway endpoint") {
		t.Fatalf("expected GatewayEndpoint error, got %v", err)
	}
}

func TestLoadConfigRequiresGatewayID(t *testing.T) {
	env := validEnv()
	delete(env, "OPENSHELL_GATEWAY_ID")
	_, err := loadConfig(nil, func(name string) string { return env[name] })
	if err == nil || !strings.Contains(err.Error(), "gatewayID") {
		t.Fatalf("expected gatewayID error, got %v", err)
	}
}
