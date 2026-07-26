package main

import (
	"strings"
	"testing"
)

func validEnv() map[string]string {
	return map[string]string{
		"OPENSHELL_EC2_AMI_ID":            "ami-test",
		"OPENSHELL_EC2_SUBNET_ID":         "subnet-test",
		"OPENSHELL_EC2_SECURITY_GROUP_ID": "sg-test",
		"OPENSHELL_GATEWAY_ENDPOINT":      "http://gateway.example",
		"OPENSHELL_GATEWAY_ID":            "gw-test",
		"OPENSHELL_INSECURE":              "true",
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
	env["OPENSHELL_EC2_MAX_INSTANCES"] = "7"
	getenv := func(key string) string { return env[key] }
	cfg, err := loadConfig([]string{"--max-instances=3"}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.driver.MaxInstances != 3 {
		t.Fatalf("flag did not override env: %d", cfg.driver.MaxInstances)
	}
	if cfg.driver.AMIID != "ami-test" {
		t.Fatalf("env was not loaded: %+v", cfg.driver)
	}
}

func TestLoadConfigRejectsInvalidEnvironmentValues(t *testing.T) {
	for key, value := range map[string]string{
		"OPENSHELL_EC2_MAX_INSTANCES": "10junk",
		"OPENSHELL_EC2_POLL_INTERVAL": "soon",
		"OPENSHELL_EC2_SPOT":          "yes",
		"OPENSHELL_EC2_DISK_SIZE_GB":  "large",
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
