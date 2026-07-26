// Package bootstrap provides provider-neutral bootstrap-data helpers.
package bootstrap

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode"
)

const (
	TLSCAPath        = "/etc/openshell/tls/client/ca.crt"
	TLSCertPath      = "/etc/openshell/tls/client/tls.crt"
	TLSKeyPath       = "/etc/openshell/tls/client/tls.key"
	SandboxTokenPath = "/etc/openshell/auth/sandbox.jwt"
	EnvironmentPath  = "/etc/openshell/supervisor.env"
	maxTLSFileSize   = 256 << 10
)

// TransportConfig is the provider-independent supervisor-to-gateway transport
// configuration. TLS is mandatory unless Insecure is explicitly enabled.
type TransportConfig struct {
	GatewayEndpoint string
	CACertPEM       []byte
	ClientCertPEM   []byte
	ClientKeyPEM    []byte
	Insecure        bool
}

// SecurityBlockParams contains the per-sandbox values rendered with a
// TransportConfig into a cloud-init write_files fragment.
type SecurityBlockParams struct {
	SandboxID    string
	SandboxToken string
	LogLevel     string
	MaxLifetime  string // validated for the surrounding platform template
}

// LoadTransportConfig reads TLS material from files and validates the complete
// configuration. Paths are deliberately kept out of driver-specific config.
func LoadTransportConfig(endpoint, caPath, certPath, keyPath string, insecure bool) (TransportConfig, error) {
	cfg := TransportConfig{GatewayEndpoint: endpoint, Insecure: insecure}
	if !insecure {
		var errs []error
		cfg.CACertPEM, errs = readRequiredFile("gateway CA certificate", caPath, errs)
		cfg.ClientCertPEM, errs = readRequiredFile("gateway client certificate", certPath, errs)
		cfg.ClientKeyPEM, errs = readRequiredFile("gateway client key", keyPath, errs)
		if len(errs) > 0 {
			return TransportConfig{}, errors.Join(errs...)
		}
	}
	if err := cfg.Validate(); err != nil {
		return TransportConfig{}, err
	}
	return cfg, nil
}

func readRequiredFile(name, path string, errs []error) ([]byte, []error) {
	if path == "" {
		return nil, append(errs, fmt.Errorf("%s path is required unless --insecure is set", name))
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, append(errs, fmt.Errorf("read %s %q: %w", name, path, err))
	}
	if len(contents) > maxTLSFileSize {
		return nil, append(errs, fmt.Errorf("%s %q exceeds %d bytes", name, path, maxTLSFileSize))
	}
	return contents, errs
}

// Validate fails fast on endpoint or certificate errors that would otherwise
// surface only after a VM boots.
func (c TransportConfig) Validate() error {
	u, err := url.ParseRequestURI(c.GatewayEndpoint)
	if err != nil || u.Hostname() == "" {
		return errors.New("gateway endpoint must be an absolute URL")
	}
	if u.User != nil || (u.EscapedPath() != "" && u.EscapedPath() != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("gateway endpoint must not contain credentials, a path, query parameters, or a fragment")
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return errors.New("gateway endpoint port must be between 1 and 65535")
		}
	}
	wantScheme := "https"
	if c.Insecure {
		wantScheme = "http"
	}
	if u.Scheme != wantScheme {
		return fmt.Errorf("gateway endpoint must use %s when insecure=%t", wantScheme, c.Insecure)
	}
	if c.Insecure {
		return nil
	}
	pool := x509.NewCertPool()
	if len(c.CACertPEM) == 0 || !pool.AppendCertsFromPEM(c.CACertPEM) {
		return errors.New("gateway CA certificate is missing or is not valid PEM")
	}
	pair, err := tls.X509KeyPair(c.ClientCertPEM, c.ClientKeyPEM)
	if err != nil {
		return fmt.Errorf("gateway client certificate/key are invalid or do not match: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse gateway client certificate: %w", err)
	}
	intermediates := x509.NewCertPool()
	for _, raw := range pair.Certificate[1:] {
		cert, err := x509.ParseCertificate(raw)
		if err != nil {
			return fmt.Errorf("parse gateway client certificate chain: %w", err)
		}
		intermediates.AddCert(cert)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return fmt.Errorf("gateway client certificate is not valid for the configured CA: %w", err)
	}
	return nil
}

// RenderSecurityBlock returns a cloud-init write_files list fragment. A driver
// template only needs to place {{.SecurityBlock}} directly below write_files:.
func (c TransportConfig) RenderSecurityBlock(params SecurityBlockParams) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	for name, value := range map[string]string{
		"sandbox ID": params.SandboxID, "sandbox token": params.SandboxToken, "log level": params.LogLevel,
		"max lifetime": params.MaxLifetime,
	} {
		if value == "" && name != "log level" {
			return "", fmt.Errorf("%s is required", name)
		}
		if strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return "", fmt.Errorf("%s contains a forbidden control character", name)
		}
	}
	env := []string{
		"OPENSHELL_SANDBOX_ID=" + strconv.Quote(params.SandboxID),
		"OPENSHELL_SANDBOX_TOKEN_FILE=" + strconv.Quote(SandboxTokenPath),
		"OPENSHELL_GATEWAY_ENDPOINT=" + strconv.Quote(c.GatewayEndpoint),
		"OPENSHELL_ENDPOINT=" + strconv.Quote(c.GatewayEndpoint),
		"OPENSHELL_LOG_LEVEL=" + strconv.Quote(params.LogLevel),
		"HTTP_PROXY=" + strconv.Quote(c.GatewayEndpoint),
		"HTTPS_PROXY=" + strconv.Quote(c.GatewayEndpoint),
	}
	type file struct{ path, content, permissions string }
	files := []file{
		{EnvironmentPath, strings.Join(env, "\n") + "\n", "0600"},
		{SandboxTokenPath, params.SandboxToken + "\n", "0600"},
	}
	if !c.Insecure {
		env = append(env,
			"OPENSHELL_TLS_CA="+strconv.Quote(TLSCAPath),
			"OPENSHELL_TLS_CERT="+strconv.Quote(TLSCertPath),
			"OPENSHELL_TLS_KEY="+strconv.Quote(TLSKeyPath),
		)
		files[0].content = strings.Join(env, "\n") + "\n"
		files = append(files,
			file{TLSCAPath, string(c.CACertPEM), "0644"},
			file{TLSCertPath, string(c.ClientCertPEM), "0600"},
			file{TLSKeyPath, string(c.ClientKeyPEM), "0600"},
		)
	}
	var out strings.Builder
	for _, f := range files {
		fmt.Fprintf(&out, "  - path: %s\n    encoding: b64\n    content: %s\n    owner: root:root\n    permissions: '%s'\n\n",
			f.path, base64.StdEncoding.EncodeToString([]byte(f.content)), f.permissions)
	}
	return strings.TrimSuffix(out.String(), "\n"), nil
}
