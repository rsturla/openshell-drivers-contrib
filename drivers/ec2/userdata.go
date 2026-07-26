package ec2

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"text/template"

	"github.com/rsturla/openshell-drivers-contrib/pkg/bootstrap"
)

const maxEC2UserDataSize = 16 << 10

// UserDataParams holds the parameters for rendering the cloud-init user data.
type UserDataParams struct {
	Transport    bootstrap.TransportConfig
	SandboxID    string
	SandboxToken string
	LogLevel     string
	MaxLifetime  string // e.g. "8h", passed to self-destruct timer
}

// cloudInitTemplate is the cloud-init YAML template for configuring an
// EC2 instance as an OpenShell sandbox.
const cloudInitTemplate = `#cloud-config
write_files:
{{.SecurityBlock}}

  - path: /etc/systemd/system/openshell-self-destruct.timer
    content: |
      [Unit]
      Description=Terminate instance after max lifetime
      [Timer]
      OnBootSec={{.MaxLifetime}}
      Unit=openshell-self-destruct.service
      [Install]
      WantedBy=timers.target
    owner: root:root
    permissions: '0644'

  - path: /etc/systemd/system/openshell-self-destruct.service
    content: |
      [Unit]
      Description=Self-terminate EC2 instance (max lifetime exceeded)
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

// RenderUserData renders the cloud-init user data template and returns it
// as a base64-encoded string suitable for EC2 RunInstances UserData.
func RenderUserData(params UserDataParams) (string, error) {
	securityBlock, err := params.Transport.RenderSecurityBlock(bootstrap.SecurityBlockParams{
		SandboxID: params.SandboxID, SandboxToken: params.SandboxToken, LogLevel: params.LogLevel, MaxLifetime: params.MaxLifetime,
	})
	if err != nil {
		return "", err
	}
	templateParams := struct {
		SecurityBlock string
		MaxLifetime   string
	}{securityBlock, params.MaxLifetime}

	tmpl, err := template.New("userdata").Parse(cloudInitTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing userdata template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, templateParams); err != nil {
		return "", fmt.Errorf("executing userdata template: %w", err)
	}
	if buf.Len() > maxEC2UserDataSize {
		return "", fmt.Errorf("userdata is %d bytes; EC2 limit is %d bytes", buf.Len(), maxEC2UserDataSize)
	}

	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
