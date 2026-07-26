# OpenShell Compute Drivers

External compute drivers for NVIDIA OpenShell. Each binary implements the
`openshell.compute.v1.ComputeDriver` gRPC contract for one compute platform.

```text
OpenShell Gateway
    └── gRPC over a private Unix socket
        ├── openshell-driver-ec2
        │   └── AWS EC2 disposable sandbox instances
        └── openshell-driver-kubevirt
            └── KubeVirt VMs on Kubernetes / OpenShift
```

## Transport security

Supervisor-to-gateway connections use mTLS plus a gateway-minted per-sandbox
JWT. The client certificate authenticates the OpenShell deployment, while the
JWT identifies and authorizes the individual sandbox. TLS is mandatory by
default; invalid or missing certificate material prevents driver startup.

Mount the `certgen` output into the driver container and configure these shared
flags for either driver:

<!-- markdownlint-disable MD013 -->

| Flag | Environment | Purpose |
| --- | --- | --- |
| `--gateway-endpoint` | `OPENSHELL_GATEWAY_ENDPOINT` | Absolute `https://` gateway URL |
| `--gateway-ca-cert` | `OPENSHELL_GATEWAY_CA_CERT` | Path to `ca.crt` |
| `--gateway-client-cert` | `OPENSHELL_GATEWAY_CLIENT_CERT` | Path to client `tls.crt` |
| `--gateway-client-key` | `OPENSHELL_GATEWAY_CLIENT_KEY` | Path to client `tls.key` |
| `--insecure` | `OPENSHELL_INSECURE` | Explicitly permit an `http://` gateway |

<!-- markdownlint-enable MD013 -->

```bash
go run ./cmd/openshell-driver-ec2 \
  --gateway-endpoint https://gateway.openshell-system.svc:8080 \
  --gateway-ca-cert /var/run/secrets/openshell-client-tls/ca.crt \
  --gateway-client-cert /var/run/secrets/openshell-client-tls/tls.crt \
  --gateway-client-key /var/run/secrets/openshell-client-tls/tls.key \
  --gateway-id production \
  --ami-id ami-0123456789abcdef0 \
  --subnet-id subnet-0123456789abcdef0 \
  --security-group-id sg-0123456789abcdef0
```

The shared `pkg/bootstrap.TransportConfig` validates the endpoint and
certificate pair, then renders a provider-neutral `SecurityBlock` into each
cloud-init template. The block writes TLS files and the sandbox JWT with
restrictive permissions; the JWT is referenced through
`OPENSHELL_SANDBOX_TOKEN_FILE` instead of being placed in the supervisor process
environment.

Use `--insecure` only with an `http://` endpoint for isolated development. The
driver emits a startup warning and another warning for every RPC while this
mode is active. This setting is independent of `--bind-address`, which retains
the loopback-only plaintext development listener for the local driver gRPC
server.

See [the transport security architecture](docs/transport-security.md), the
[hardened gateway ConfigMap](docs/gateway-configmap.yaml), and the
[KubeVirt egress NetworkPolicy](docs/kubevirt-network-policy.yaml).

## EC2 driver

### EC2 safety model

The EC2 driver treats instances as disposable and enforces several independent
cleanup layers:

- Create reserves an in-process capacity slot atomically before calling AWS.
- A stable EC2 `ClientToken` makes gateway and process retries idempotent.
- Every instance is tagged with its gateway, sandbox,
  creation time, and maximum lifetime.
- Startup performs complete, paginated reconciliation before serving requests.
- Expired instances remain tracked until termination is
  accepted and later observed.
- Instance-initiated shutdown is configured to **terminate**, so the cloud-init
  systemd timer remains effective when the gateway and driver are unavailable.
- IMDS is disabled, no instance profile is attached, the
  root disk is encrypted, and it is deleted on termination.

The Go provider interface is not a security boundary. The driver's AWS identity
must also be restricted with IAM and the configured security group must limit
egress to the gateway proxy.

### EC2 configuration

EC2-specific flags. Flags override environment variables. Invalid environment
values fail startup; they never silently fall back to defaults.

<!-- markdownlint-disable MD013 -->

| Flag | Environment | Default | Required |
| --- | --- | ---: | --- |
| `--socket` | `OPENSHELL_DRIVER_SOCKET` | `/run/openshell/ec2.sock` | Production |
| `--bind-address` | `OPENSHELL_DRIVER_BIND` | empty | Development only |
| `--ami-id` | `OPENSHELL_EC2_AMI_ID` | empty | Yes |
| `--instance-type` | `OPENSHELL_EC2_INSTANCE_TYPE` | `c7i.4xlarge` | Yes |
| `--subnet-id` | `OPENSHELL_EC2_SUBNET_ID` | empty | Yes |
| `--security-group-id` | `OPENSHELL_EC2_SECURITY_GROUP_ID` | empty | Yes |
| `--key-name` | `OPENSHELL_EC2_KEY_NAME` | empty | No; debugging only |
| `--gateway-endpoint` | `OPENSHELL_GATEWAY_ENDPOINT` | empty | Yes; HTTPS unless insecure |
| `--gateway-ca-cert` | `OPENSHELL_GATEWAY_CA_CERT` | empty | Yes unless insecure |
| `--gateway-client-cert` | `OPENSHELL_GATEWAY_CLIENT_CERT` | empty | Yes unless insecure |
| `--gateway-client-key` | `OPENSHELL_GATEWAY_CLIENT_KEY` | empty | Yes unless insecure |
| `--insecure` | `OPENSHELL_INSECURE` | `false` | No; development only |
| `--gateway-id` | `OPENSHELL_GATEWAY_ID` | empty | Yes; unique per deployment |
| `--region` | `AWS_REGION` | `us-east-1` | Yes |
| `--poll-interval` | `OPENSHELL_EC2_POLL_INTERVAL` | `15s` | Yes |
| `--spot` | `OPENSHELL_EC2_SPOT` | `false` | No |
| `--disk-size-gb` | `OPENSHELL_EC2_DISK_SIZE_GB` | `200` | Yes |
| `--max-instances` | `OPENSHELL_EC2_MAX_INSTANCES` | `10` | Yes |
| `--max-instance-age` | `OPENSHELL_EC2_MAX_INSTANCE_AGE` | `8h` | Yes |

<!-- markdownlint-enable MD013 -->

Plaintext TCP is restricted to loopback. Production deployments should mount a
private shared directory into the gateway and driver containers and use the
Unix socket, which is created with mode `0600`. Do not expose the socket to
untrusted sidecars.

Example local invocation:

```bash
go run ./cmd/openshell-driver-ec2 \
  --bind-address 127.0.0.1:50051 \
  --ami-id ami-0123456789abcdef0 \
  --subnet-id subnet-0123456789abcdef0 \
  --security-group-id sg-0123456789abcdef0 \
  --gateway-endpoint http://127.0.0.1:8080 \
  --insecure \
  --gateway-id development-alice
```

AWS credentials are loaded with the standard AWS SDK
credential chain. The production identity should not have
`iam:PassRole`, and the driver never attaches an instance
profile. See [the IAM policy template](docs/ec2-iam-policy.json).
Replace its placeholders and validate the result with IAM
Access Analyzer or the policy simulator before deployment.

### Operations and troubleshooting

- `initial driver reconciliation` failures prevent readiness and serving. Check
  IAM, region, and EC2 API reachability.
- `AWS denied the EC2 operation` indicates missing IAM authorization or an IAM
  condition that does not match the configured AMI/network/tags.
- `watcher poll failed` includes gateway, region,
  consecutive failure count, and next retry. Persistent
  failures mean max-age cleanup is relying on the instance
  timer and external tag sweeper.
- `watcher fell behind` causes the gateway to reconnect and receive a complete
  snapshot; events are not silently dropped.
- Stop and termination logs say `requested` until EC2 reconciliation confirms
  the final state.

The shared `pkg/server` handles initialization, listener hardening, lifecycle,
and bounded shutdown. `pkg/safeguards` provides immutable records, atomic
capacity leases, expiration selection, and loss-aware event subscriptions.

## KubeVirt driver

### KubeVirt configuration

<!-- markdownlint-disable MD013 -->

| Flag | Environment | Default | Required |
| --- | --- | ---: | --- |
| `--socket` | `OPENSHELL_DRIVER_SOCKET` | `/run/openshell/kubevirt.sock` | Production |
| `--bind-address` | `OPENSHELL_DRIVER_BIND` | empty | Development only |
| `--namespace` | `OPENSHELL_KUBEVIRT_NAMESPACE` | `openshell` | Yes |
| `--boot-source` | `OPENSHELL_KUBEVIRT_BOOT_SOURCE` | `fedora` | Yes |
| `--boot-source-namespace` | `OPENSHELL_KUBEVIRT_BOOT_SOURCE_NAMESPACE` | `openshift-virtualization-os-images` | Yes |
| `--default-instancetype` | `OPENSHELL_KUBEVIRT_DEFAULT_INSTANCETYPE` | `cx1.4xlarge` | Yes |
| `--default-preference` | `OPENSHELL_KUBEVIRT_DEFAULT_PREFERENCE` | empty | No |
| `--storage-class` | `OPENSHELL_KUBEVIRT_STORAGE_CLASS` | empty | Yes |
| `--storage-label` | | empty | No; `key=value` for PVC labels (e.g. appcode) |
| `--storage-annotation` | | empty | No; `key=value` for PVC annotations (e.g. reclaimPolicy) |
| `--gateway-endpoint` | `OPENSHELL_GATEWAY_ENDPOINT` | empty | Yes; HTTPS unless insecure |
| `--gateway-ca-cert` | `OPENSHELL_GATEWAY_CA_CERT` | empty | Yes unless insecure |
| `--gateway-client-cert` | `OPENSHELL_GATEWAY_CLIENT_CERT` | empty | Yes unless insecure |
| `--gateway-client-key` | `OPENSHELL_GATEWAY_CLIENT_KEY` | empty | Yes unless insecure |
| `--insecure` | `OPENSHELL_INSECURE` | `false` | No; development only |
| `--gateway-id` | `OPENSHELL_GATEWAY_ID` | empty | Yes; unique per deployment |
| `--disk-size` | `OPENSHELL_KUBEVIRT_DISK_SIZE` | `100Gi` | Yes |
| `--poll-interval` | `OPENSHELL_KUBEVIRT_POLL_INTERVAL` | `15s` | Yes |
| `--max-instances` | `OPENSHELL_KUBEVIRT_MAX_INSTANCES` | `10` | Yes |
| `--max-instance-age` | `OPENSHELL_KUBEVIRT_MAX_INSTANCE_AGE` | `8h` | Yes |
| `--kubeconfig` | `KUBECONFIG` | empty | No; out-of-cluster development only |

<!-- markdownlint-enable MD013 -->

The `u1` (universal) instancetype series works on all worker nodes. The `cx1`
and `d1` series require dedicated CPU placement and huge pages, which limits
scheduling to bare-metal or specially configured nodes.

### KubeVirt safety model

The KubeVirt driver uses a fixed, typed `kubevirt.io/v1` VirtualMachine policy.
VMs use `RerunOnFailure`, a trusted cluster instancetype/preference, a
DataSource-backed DataVolume, pod masquerade networking, and an immutable
cloud-init Secret. Request `platform_config` and `driver_config` are rejected;
they are never merged into the VM.

`--namespace` is the dedicated Kubernetes resource namespace. The sandbox
namespace received over gRPC is metadata only. `--storage-class` is required
and must name an operator-approved encrypted StorageClass. VM, Secret, and
DataVolume names are deterministic, so retries after ambiguous API failures
resolve the existing request instead of creating duplicates.

The persistent guest timer uses the reservation's absolute expiry timestamp, so
a reboot cannot reset the maximum lifetime. It stops compute, but deletion
is confirmed only when the VM CR is absent. The reconciler retains stopped,
failed, and deleting VMs until that confirmation. Deploy the namespace with
least-privilege RBAC and CNI-validated egress restrictions;
[`docs/kubevirt-security.yaml`](docs/kubevirt-security.yaml) is the reference
policy. Do not grant unrelated principals permission to create Pods or other
workloads in the VM namespace, because that implicitly grants the ability to
mount its cloud-init Secrets.
