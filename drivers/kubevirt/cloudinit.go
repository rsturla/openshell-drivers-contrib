package kubevirt

import (
	"bytes"
	"fmt"
	"text/template"
	"time"

	"github.com/rsturla/openshell-drivers-contrib/pkg/bootstrap"
)

const maxKubeVirtCloudInitSize = 900 << 10

// CloudInitParams holds the parameters for rendering cloud-init userdata.
type CloudInitParams struct {
	Transport    bootstrap.TransportConfig
	SandboxID    string
	SandboxToken string
	LogLevel     string
	MaxLifetime  string // retained in metadata and validated with the bootstrap inputs
	ExpiresAt    time.Time
}

// cloudInitTemplate is the cloud-init YAML template for configuring a KubeVirt
// VM as an OpenShell sandbox.
const cloudInitTemplate = `#cloud-config
write_files:
{{.SecurityBlock}}

  - path: /etc/systemd/system/openshell-self-destruct.timer
    content: |
      [Unit]
      Description=Terminate VM after max lifetime
      [Timer]
      OnCalendar={{.ExpiresAt}}
      Persistent=true
      Unit=openshell-self-destruct.service
      [Install]
      WantedBy=timers.target
    owner: root:root
    permissions: '0644'

  - path: /etc/systemd/system/openshell-self-destruct.service
    content: |
      [Unit]
      Description=Self-terminate VM (max lifetime exceeded)
      [Service]
      Type=oneshot
      ExecStart=/usr/sbin/shutdown -h now "openshell: max instance lifetime reached"
    owner: root:root
    permissions: '0644'

runcmd:
  - systemctl daemon-reload
  - systemctl enable --now openshell-self-destruct.timer
  - systemctl enable --now openshell-supervisor.service
`

// RenderCloudInit renders the cloud-init userdata for a KubeVirt VM.
// Unlike EC2, the result is returned as plain text (not base64-encoded)
// because KubeVirt's CloudInitNoCloud accepts raw userdata via a Secret.
func RenderCloudInit(params CloudInitParams) (string, error) {
	if params.ExpiresAt.IsZero() {
		return "", fmt.Errorf("self-destruct expiry is required")
	}
	securityBlock, err := params.Transport.RenderSecurityBlock(bootstrap.SecurityBlockParams{
		SandboxID: params.SandboxID, SandboxToken: params.SandboxToken, LogLevel: params.LogLevel, MaxLifetime: params.MaxLifetime,
	})
	if err != nil {
		return "", err
	}
	templateParams := struct {
		SecurityBlock string
		ExpiresAt     string
	}{securityBlock, params.ExpiresAt.UTC().Format("2006-01-02 15:04:05 UTC")}

	tmpl, err := template.New("cloudinit").Parse(cloudInitTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing cloud-init template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, templateParams); err != nil {
		return "", fmt.Errorf("executing cloud-init template: %w", err)
	}
	if buf.Len() > maxKubeVirtCloudInitSize {
		return "", fmt.Errorf("cloud-init is %d bytes; safe Kubernetes Secret limit is %d bytes", buf.Len(), maxKubeVirtCloudInitSize)
	}

	return buf.String(), nil
}
