package kubevirt

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/rsturla/openshell-drivers-contrib/pkg/bootstrap"
	"sigs.k8s.io/yaml"
)

var testExpiry = time.Date(2026, 7, 25, 20, 0, 0, 0, time.UTC)

func insecureTransport(endpoint string) bootstrap.TransportConfig {
	return bootstrap.TransportConfig{GatewayEndpoint: endpoint, Insecure: true}
}

func environmentFromCloudInit(t *testing.T, cloud string) string {
	t.Helper()
	var parsed struct {
		WriteFiles []struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		} `json:"write_files"`
	}
	if err := yaml.Unmarshal([]byte(cloud), &parsed); err != nil {
		t.Fatalf("cloud-init is not valid YAML: %v\n%s", err, cloud)
	}
	for _, f := range parsed.WriteFiles {
		if f.Path == bootstrap.EnvironmentPath {
			decoded, err := base64.StdEncoding.DecodeString(f.Content)
			if err != nil {
				t.Fatalf("decode environment: %v", err)
			}
			return string(decoded)
		}
	}
	t.Fatal("environment file missing")
	return ""
}

func TestRenderCloudInitContainsExpectedFields(t *testing.T) {
	cloud, err := RenderCloudInit(CloudInitParams{
		Transport:    insecureTransport("http://gateway.example:8080"),
		SandboxID:    "sandbox",
		SandboxToken: "token/+==",
		LogLevel:     "debug",
		MaxLifetime:  "8h0m0s",
		ExpiresAt:    testExpiry,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cloud, "#cloud-config") {
		t.Fatal("missing #cloud-config header")
	}
	env := environmentFromCloudInit(t, cloud)
	for _, expected := range []string{
		`OPENSHELL_SANDBOX_ID="sandbox"`,
		`OPENSHELL_SANDBOX_TOKEN_FILE="/etc/openshell/auth/sandbox.jwt"`,
		`OPENSHELL_GATEWAY_ENDPOINT="http://gateway.example:8080"`,
		`OPENSHELL_LOG_LEVEL="debug"`,
		`HTTP_PROXY="http://gateway.example:8080"`,
		`HTTPS_PROXY="http://gateway.example:8080"`,
	} {
		if !strings.Contains(env, expected) {
			t.Errorf("missing %q in cloud-init output:\n%s", expected, cloud)
		}
	}
	for _, expected := range []string{"OnCalendar=2026-07-25 20:00:00 UTC", "Persistent=true", "openshell-self-destruct.timer", "openshell-self-destruct.service", "openshell-supervisor.service"} {
		if !strings.Contains(cloud, expected) {
			t.Errorf("missing %q in cloud-init", expected)
		}
	}
}

func TestRenderCloudInitProducesParseableExactEnvironmentFile(t *testing.T) {
	cloud, err := RenderCloudInit(CloudInitParams{
		Transport: insecureTransport("http://gw.example"), SandboxID: "sb", SandboxToken: `dollar$ percent% quote\" slash\\ unicode-✓`,
		LogLevel: "info", MaxLifetime: "8h", ExpiresAt: testExpiry,
	})
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		WriteFiles []struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		} `json:"write_files"`
	}
	if err := yaml.Unmarshal([]byte(cloud), &parsed); err != nil {
		t.Fatalf("cloud-init is not valid YAML: %v\n%s", err, cloud)
	}
	if len(parsed.WriteFiles) != 4 || parsed.WriteFiles[0].Path != "/etc/openshell/supervisor.env" {
		t.Fatalf("unexpected write_files: %+v", parsed.WriteFiles)
	}
	decoded, err := base64.StdEncoding.DecodeString(parsed.WriteFiles[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`OPENSHELL_SANDBOX_ID="sb"`,
		`OPENSHELL_SANDBOX_TOKEN_FILE="/etc/openshell/auth/sandbox.jwt"`,
		`OPENSHELL_GATEWAY_ENDPOINT="http://gw.example"`,
	} {
		if !strings.Contains(string(decoded), expected) {
			t.Errorf("environment content missing %q: %q", expected, decoded)
		}
	}
}

func TestRenderCloudInitIsPlainText(t *testing.T) {
	cloud, err := RenderCloudInit(CloudInitParams{
		Transport:    insecureTransport("http://gw.example"),
		SandboxID:    "sb",
		SandboxToken: "tok",
		LogLevel:     "info",
		MaxLifetime:  "4h",
		ExpiresAt:    testExpiry,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Unlike EC2, the output should NOT be base64-encoded.
	if !strings.HasPrefix(cloud, "#cloud-config") {
		t.Fatal("output should be plain text YAML, not base64")
	}
}

func TestRenderCloudInitRequiresAbsoluteExpiry(t *testing.T) {
	if _, err := RenderCloudInit(CloudInitParams{MaxLifetime: "8h"}); err == nil {
		t.Fatal("expected missing absolute expiry to be rejected")
	}
}

func TestRenderCloudInitRejectsStructuralInjection(t *testing.T) {
	for field, params := range map[string]CloudInitParams{
		"token":    {Transport: insecureTransport("http://good"), SandboxID: "sb", SandboxToken: "x\nruncmd:\n - touch /root/pwned", MaxLifetime: "8h", ExpiresAt: testExpiry},
		"ID":       {Transport: insecureTransport("http://good"), SandboxID: "x\nwrite_files:", SandboxToken: "tok", MaxLifetime: "8h", ExpiresAt: testExpiry},
		"endpoint": {Transport: insecureTransport("http://good\n- evil"), SandboxID: "sb", SandboxToken: "tok", MaxLifetime: "8h", ExpiresAt: testExpiry},
		"duration": {Transport: insecureTransport("http://good"), SandboxID: "sb", SandboxToken: "tok", MaxLifetime: "8h\nruncmd:", ExpiresAt: testExpiry},
		"loglevel": {Transport: insecureTransport("http://good"), SandboxID: "sb", SandboxToken: "tok", LogLevel: "info\nruncmd:", MaxLifetime: "8h", ExpiresAt: testExpiry},
	} {
		t.Run(field, func(t *testing.T) {
			if _, err := RenderCloudInit(params); err == nil {
				t.Fatal("expected control-character rejection")
			}
		})
	}
}

func TestRenderCloudInitHandlesSpecialCharsInToken(t *testing.T) {
	cloud, err := RenderCloudInit(CloudInitParams{
		Transport:    insecureTransport("http://gw.example"),
		SandboxID:    "sb-1",
		SandboxToken: `to"ken/with+sp3c!al=chars`,
		LogLevel:     "info",
		MaxLifetime:  "8h",
		ExpiresAt:    testExpiry,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cloud, bootstrap.SandboxTokenPath) {
		t.Fatal("token file missing")
	}
	if strings.Contains(cloud, `to"ken/with+sp3c!al=chars`) {
		t.Fatal("token appeared in plaintext cloud-init")
	}
}
