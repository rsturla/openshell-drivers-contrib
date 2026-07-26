# Transport Security Architecture

The compute drivers bootstrap an authenticated, encrypted
supervisor connection to the OpenShell gateway while keeping
platform-specific VM creation separate from transport policy.

## Certificate and token flow

```text
certgen Job
  ├─ openshell-server-tls ──mounted──> gateway
  ├─ openshell-client-tls ──mounted──> driver flags
  │                                      │
  │                         pkg/bootstrap.TransportConfig
  │                                      │
  └─ CA certificate ────────────────> SecurityBlock
                                         │
Gateway JWT issuer ──sandbox_token──> ComputeDriver RPC
                                         │
                          EC2 UserData / KubeVirt Secret
                                         │
                              cloud-init write_files
                                         │
                         /etc/openshell/tls/client/*
                         /etc/openshell/auth/sandbox.jwt
                         /etc/openshell/supervisor.env
                                         │
                              supervisor ──mTLS+JWT──> gateway
```

The driver reads certificate files at startup. In Kubernetes,
project keys from `openshell-client-tls` into the driver
container and pass their mounted paths. For EC2 deployments,
use the deployment's secret store to mount the same files
read-only. Certificate or endpoint errors fail driver startup
before any VM is created.

The gateway configuration sets `require_client_auth = true`,
mounts `openshell-jwt-keys`, rejects unauthenticated users,
and gives sandbox tokens a positive lifetime. Keep its
`gateway_id` stable and deployment-unique. Until
platform-attested re-bootstrap exists, configure the original
token lifetime above the driver's maximum instance age; the
supervisor refreshes a running session in memory, but a
restart cannot use an expired static token file.

## Decision: mTLS plus sandbox JWT

Use both gateway mTLS and the gateway-minted sandbox JWT.

- The gateway server certificate and CA provide server
  authentication and confidentiality.
- The install-time client certificate proves that the caller
  is an approved OpenShell workload. It is a deployment
  identity, not a unique sandbox identity.
- The short-lived JWT remains the per-sandbox identity
  because its claims are already bound to the sandbox ID and
  enforced by the gateway.

The existing `certgen` client certificate is shared, so mTLS
alone cannot distinguish sandboxes. Minting per-sandbox
certificates would require a new issuance and revocation API
in the gateway and is outside the driver bootstrap boundary.
Server-only TLS plus JWT would protect the bearer token, but
would discard the gateway's existing client-authentication
layer without reducing token exposure in boot data.

## Shared package API

The provider-neutral implementation lives in `pkg/bootstrap`:

<!-- markdownlint-disable MD013 -->

```go
type TransportConfig struct {
    GatewayEndpoint string
    CACertPEM       []byte
    ClientCertPEM   []byte
    ClientKeyPEM    []byte
    Insecure        bool
}

type SecurityBlockParams struct {
    SandboxID    string
    SandboxToken string
    LogLevel     string
    MaxLifetime  string
}

func LoadTransportConfig(endpoint, caPath, certPath, keyPath string, insecure bool) (TransportConfig, error)
func (c TransportConfig) Validate() error
func (c TransportConfig) RenderSecurityBlock(params SecurityBlockParams) (string, error)
```

<!-- markdownlint-enable MD013 -->

Driver templates compose the result directly:

```yaml
write_files:
{{.SecurityBlock}}
```

The block writes the CA, client certificate, client key,
sandbox token, and supervisor environment with restrictive
modes. It sets `OPENSHELL_TLS_CA`, `OPENSHELL_TLS_CERT`,
`OPENSHELL_TLS_KEY`, and `OPENSHELL_SANDBOX_TOKEN_FILE`. EC2
base64-encodes the complete UserData document; KubeVirt stores
the document in its immutable cloud-init Secret.

## Token bootstrap decision

Keep the sandbox JWT in cloud-init until the gateway supports
a platform-identity exchange for both providers. Moving the
token from an environment variable to
`/etc/openshell/auth/sandbox.jwt` mode `0600` prevents routine
process-environment disclosure, but does not remove it from
EC2 UserData or the KubeVirt Secret.

A post-boot fetch without another credential would only move
the bearer-secret problem. A future design can exchange an EC2
instance identity document or a projected KubeVirt
service-account token for a short-lived sandbox JWT. That
requires gateway validation, replay controls, audience
binding, and sandbox-to-platform identity binding.

## Framework integration

- `pkg/bootstrap` owns loading, certificate validation,
  endpoint policy, and cloud-init rendering.
- Driver `Config` values contain a validated
  `bootstrap.TransportConfig`.
- EC2 and KubeVirt pass only sandbox-specific values to
  `RenderSecurityBlock`.
- `pkg/server` keeps the local Unix-socket gRPC listener
  unchanged. When insecure gateway transport is enabled, its
  interceptor logs a warning for every driver RPC.
- `pkg/safeguards` continues to redact `sandbox_token` from
  stored records and responses. Transport configuration never
  enters records or events.

Plaintext requires both an `http://` endpoint and
`--insecure`. Secure mode requires an `https://` endpoint and
all three certificate paths. Loopback `--bind-address` remains
a separate development-only setting for the local driver gRPC
server.

## Security findings

### [HIGH] Secrets — Sandbox JWT remains readable from provider boot data

**File:** `pkg/bootstrap/transport.go`
**Problem:** EC2 principals with
`ec2:DescribeInstanceAttribute` and Kubernetes principals with
Secret read access can recover a live sandbox bearer token.
Base64 is encoding, not encryption.
**Fix:** Restrict those permissions, shorten JWT lifetime,
delete KubeVirt cloud-init Secrets promptly, and add a gateway
platform-identity exchange before removing the token from boot
data.

### [MEDIUM] Authentication — Client certificate is deployment-wide

**File:** `pkg/bootstrap/transport.go`
**Problem:** Compromise of one VM exposes the shared client
private key, allowing another caller to satisfy the mTLS
layer. The sandbox JWT still gates sandbox identity.
**Fix:** Add gateway-backed per-sandbox certificate issuance
or platform-attested token exchange with short-lived
credentials and revocation.

### [MEDIUM] Authorization — KubeVirt Secret readers expose sandbox credentials

**File:** `docs/kubevirt-security.yaml`
**Problem:** Kubernetes Secret read permission in the VM
namespace exposes cloud-init tokens and the shared client key.
Pod creation permission can also mount those Secrets
indirectly.
**Fix:** Dedicate the namespace, grant the driver service
account only its required Secret verbs, deny unrelated
workload creation, and audit Secret access.

### [MEDIUM] Supply chain — Kubernetes dependency minors are skewed

**File:** `go.mod`
**Problem:** Direct Kubernetes modules use v0.36.3 while
KubeVirt v1.8.4 selects Kubernetes v0.34.3 transitive
modules. Cross-minor Kubernetes libraries are not a supported
compatibility set.
**Fix:** Align direct Kubernetes modules with the
KubeVirt-supported minor, or upgrade KubeVirt to a release
supporting v0.36, then run cluster integration tests.
