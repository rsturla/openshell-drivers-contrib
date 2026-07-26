package bootstrap

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testTransport(t *testing.T) TransportConfig {
	return testTransportWithClient(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
}

func testTransportWithClient(t *testing.T, notBefore, notAfter time.Time, usages []x509.ExtKeyUsage) TransportConfig {
	t.Helper()
	now := time.Now()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	clientTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "openshell-client"}, NotBefore: notBefore, NotAfter: notAfter, ExtKeyUsage: usages}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return TransportConfig{
		GatewayEndpoint: "https://gateway.example:8443",
		CACertPEM:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		ClientCertPEM:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}),
		ClientKeyPEM:    pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(clientKey)}),
	}
}

func TestTransportValidateSecureByDefault(t *testing.T) {
	cfg := testTransport(t)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := cfg
	bad.ClientKeyPEM = cfg.CACertPEM
	if err := bad.Validate(); err == nil {
		t.Fatal("expected invalid client key rejection")
	}
	if err := (TransportConfig{GatewayEndpoint: "http://gateway.example"}).Validate(); err == nil {
		t.Fatal("expected plaintext rejection")
	}
	wrongCA := cfg
	wrongCA.CACertPEM = testTransport(t).CACertPEM
	if err := wrongCA.Validate(); err == nil {
		t.Fatal("expected wrong CA rejection")
	}
	expired := testTransportWithClient(t, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour), []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	if err := expired.Validate(); err == nil {
		t.Fatal("expected expired certificate rejection")
	}
	wrongUsage := testTransportWithClient(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	if err := wrongUsage.Validate(); err == nil {
		t.Fatal("expected wrong EKU rejection")
	}
}

func TestTransportRejectsNonOriginEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"https://user:pass@gateway.example", "https://gateway.example/rpc", "https://gateway.example?x=1",
		"https://gateway.example#fragment", "https://:443", "https://gateway.example:70000",
	} {
		t.Run(endpoint, func(t *testing.T) {
			cfg := testTransport(t)
			cfg.GatewayEndpoint = endpoint
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected endpoint rejection")
			}
		})
	}
}

func TestRenderSecurityBlockWritesTLSAndTokenFile(t *testing.T) {
	block, err := testTransport(t).RenderSecurityBlock(SecurityBlockParams{SandboxID: "sandbox", SandboxToken: `quote"/token`, LogLevel: "info", MaxLifetime: "8h"})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{EnvironmentPath, SandboxTokenPath, TLSCAPath, TLSCertPath, TLSKeyPath, "permissions: '0600'"} {
		if !strings.Contains(block, expected) {
			t.Errorf("missing %q in block", expected)
		}
	}
	if strings.Contains(block, `quote"/token`) {
		t.Fatal("token appeared in plaintext")
	}
	lines := strings.Split(block, "\n")
	var envEncoded string
	for i, line := range lines {
		if strings.Contains(line, EnvironmentPath) && i+2 < len(lines) {
			envEncoded = strings.TrimPrefix(strings.TrimSpace(lines[i+2]), "content: ")
		}
	}
	env, err := base64.StdEncoding.DecodeString(envEncoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"OPENSHELL_SANDBOX_TOKEN_FILE=", "OPENSHELL_TLS_CA=", "OPENSHELL_TLS_CERT=", "OPENSHELL_TLS_KEY="} {
		if !strings.Contains(string(env), expected) {
			t.Errorf("missing %q in environment", expected)
		}
	}
	if strings.Contains(string(env), "OPENSHELL_SANDBOX_TOKEN=") {
		t.Fatal("token must not be stored in the process environment")
	}
}

func TestLoadTransportConfigReadsFiles(t *testing.T) {
	want := testTransport(t)
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "ca.crt"), filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")}
	for i, contents := range [][]byte{want.CACertPEM, want.ClientCertPEM, want.ClientKeyPEM} {
		if err := os.WriteFile(paths[i], contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := LoadTransportConfig(want.GatewayEndpoint, paths[0], paths[1], paths[2], false)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.CACertPEM) != string(want.CACertPEM) {
		t.Fatal("CA contents differ")
	}
}

func TestInsecureTransportRequiresHTTPAndOmitsTLS(t *testing.T) {
	cfg := TransportConfig{GatewayEndpoint: "http://127.0.0.1:8080", Insecure: true}
	block, err := cfg.RenderSecurityBlock(SecurityBlockParams{SandboxID: "sb", SandboxToken: "token", LogLevel: "debug", MaxLifetime: "8h"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(block, TLSCAPath) {
		t.Fatal("insecure block contains TLS material")
	}
	if err := (TransportConfig{GatewayEndpoint: "https://gateway.example", Insecure: true}).Validate(); err == nil {
		t.Fatal("expected insecure HTTPS mismatch")
	}
}

func TestRenderSecurityBlockRejectsControlCharacters(t *testing.T) {
	cfg := TransportConfig{GatewayEndpoint: "http://gateway.example", Insecure: true}
	if _, err := cfg.RenderSecurityBlock(SecurityBlockParams{SandboxID: "sb", SandboxToken: "token\ninjected", MaxLifetime: "8h"}); err == nil {
		t.Fatal("expected control-character rejection")
	}
}
