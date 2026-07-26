package ec2

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/rsturla/openshell-drivers-contrib/pkg/bootstrap"
)

func insecureTransport(endpoint string) bootstrap.TransportConfig {
	return bootstrap.TransportConfig{GatewayEndpoint: endpoint, Insecure: true}
}

func TestRenderUserDataEncodesEnvironmentFile(t *testing.T) {
	encoded, err := RenderUserData(UserDataParams{Transport: insecureTransport("http://gateway.example:8080"), SandboxID: "sandbox", SandboxToken: "token/+==", LogLevel: "debug", MaxLifetime: "8h0m0s"})
	if err != nil {
		t.Fatal(err)
	}
	cloudBytes, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	cloud := string(cloudBytes)
	if !strings.HasPrefix(cloud, "#cloud-config") || !strings.Contains(cloud, "encoding: b64") || !strings.Contains(cloud, "OnBootSec=8h0m0s") {
		t.Fatalf("unexpected cloud config:\n%s", cloud)
	}
	var content string
	for _, line := range strings.Split(cloud, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "content: ") {
			content = strings.TrimPrefix(strings.TrimSpace(line), "content: ")
			break
		}
	}
	envBytes, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		t.Fatalf("environment payload: %v", err)
	}
	env := string(envBytes)
	for _, expected := range []string{`OPENSHELL_SANDBOX_ID="sandbox"`, `OPENSHELL_SANDBOX_TOKEN_FILE="/etc/openshell/auth/sandbox.jwt"`, `HTTP_PROXY="http://gateway.example:8080"`} {
		if !strings.Contains(env, expected) {
			t.Errorf("missing %q in %q", expected, env)
		}
	}
	if strings.Contains(cloud, "token/+==") {
		t.Fatal("token appeared unencoded in YAML")
	}
}

func TestRenderUserDataRejectsStructuralInjection(t *testing.T) {
	for field, params := range map[string]UserDataParams{
		"token":    {Transport: insecureTransport("http://good"), SandboxID: "sb", SandboxToken: "x\nruncmd:\n - touch /root/pwned", MaxLifetime: "8h"},
		"ID":       {Transport: insecureTransport("http://good"), SandboxID: "x\nwrite_files:", SandboxToken: "tok", MaxLifetime: "8h"},
		"endpoint": {Transport: insecureTransport("http://good\n- evil"), SandboxID: "sb", SandboxToken: "tok", MaxLifetime: "8h"},
		"duration": {Transport: insecureTransport("http://good"), SandboxID: "sb", SandboxToken: "tok", MaxLifetime: "8h\nruncmd:"},
	} {
		t.Run(field, func(t *testing.T) {
			if _, err := RenderUserData(params); err == nil {
				t.Fatal("expected control-character rejection")
			}
		})
	}
}
