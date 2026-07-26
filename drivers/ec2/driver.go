package ec2

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/aws/smithy-go"
	"github.com/rsturla/openshell-drivers-contrib/gen/computepb"
	"github.com/rsturla/openshell-drivers-contrib/pkg/quantity"
	"github.com/rsturla/openshell-drivers-contrib/pkg/safeguards"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	DriverName   = "openshell-driver-ec2"
	DefaultImage = "ghcr.io/nvidia/openshell/sandbox:0.0.91"

	TagSandboxID   = "openshell.io/sandbox-id"
	TagSandboxName = "openshell.io/sandbox-name"
	TagNamespace   = "openshell.io/namespace"
	TagGatewayID   = "openshell.io/gateway-id"
	TagWorkspace   = "openshell.io/workspace"
	TagCreatedAt   = "openshell.io/created-at"
	TagMaxLifetime = "openshell.io/max-lifetime"
)

// DriverVersion is overridden at build time with -ldflags.
var DriverVersion = "devel"

type EC2Driver struct {
	computepb.UnimplementedComputeDriverServer
	config   Config
	provider InstanceProvider
	registry *safeguards.Registry
}

func NewEC2Driver(config Config, provider InstanceProvider) (*EC2Driver, error) {
	if provider == nil {
		return nil, errors.New("instance provider is required")
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid EC2 driver configuration: %w", err)
	}
	return &EC2Driver{
		config: config, provider: provider,
		registry: safeguards.NewRegistry(safeguards.Limits{MaxInstances: config.MaxInstances, MaxInstanceAge: config.MaxInstanceAge}),
	}, nil
}

func (d *EC2Driver) GetCapabilities(context.Context, *computepb.GetCapabilitiesRequest) (*computepb.GetCapabilitiesResponse, error) {
	return &computepb.GetCapabilitiesResponse{DriverName: DriverName, DriverVersion: DriverVersion, DefaultImage: DefaultImage}, nil
}

func (d *EC2Driver) ValidateSandboxCreate(_ context.Context, req *computepb.ValidateSandboxCreateRequest) (*computepb.ValidateSandboxCreateResponse, error) {
	if err := validateSandbox(req.GetSandbox()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &computepb.ValidateSandboxCreateResponse{}, nil
}

func validateSandbox(sb *computepb.DriverSandbox) error {
	if sb == nil {
		return errors.New("sandbox is required")
	}
	if err := validateText("sandbox ID", sb.GetId(), true, 256); err != nil {
		return err
	}
	if err := validateText("sandbox name", sb.GetName(), true, 246); err != nil {
		return err
	}
	if err := validateText("namespace", sb.GetNamespace(), false, 256); err != nil {
		return err
	}
	if err := validateText("workspace", sb.GetWorkspace(), false, 256); err != nil {
		return err
	}
	spec := sb.GetSpec()
	if spec == nil {
		return errors.New("sandbox spec is required")
	}
	if spec.GetTemplate() == nil {
		return errors.New("sandbox template is required")
	}
	if err := validateText("sandbox token", spec.GetSandboxToken(), false, 16*1024); err != nil {
		return err
	}
	if level := spec.GetLogLevel(); level != "" && level != "debug" && level != "info" && level != "warn" && level != "error" {
		return fmt.Errorf("unsupported log level %q", level)
	}
	if gpu := spec.GetResourceRequirements().GetGpu(); gpu != nil && gpu.GetCount() > 0 {
		return errors.New("GPU resources are not supported by the EC2 driver")
	}
	resources := spec.GetTemplate().GetResources()
	if resources != nil {
		if err := quantity.ValidateRequirements(resources.GetCpuRequest(), resources.GetCpuLimit(), resources.GetMemoryRequest(), resources.GetMemoryLimit()); err != nil {
			return err
		}
	}
	return nil
}

func validateText(name, value string, required bool, max int) error {
	if required && value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > max {
		return fmt.Errorf("%s exceeds %d bytes", name, max)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s contains a forbidden control character", name)
	}
	return nil
}

func (d *EC2Driver) CreateSandbox(ctx context.Context, req *computepb.CreateSandboxRequest) (*computepb.CreateSandboxResponse, error) {
	sb := req.GetSandbox()
	if err := validateSandbox(sb); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	instanceType, err := SelectInstanceType(sb.GetSpec().GetTemplate().GetResources(), d.config.DefaultInstance)
	if err != nil {
		code := codes.InvalidArgument
		if errors.Is(err, ErrNoInstanceType) {
			code = codes.ResourceExhausted
		}
		return nil, status.Error(code, err.Error())
	}
	createdAt := time.Now().UTC()
	rec := safeguards.Record{
		SandboxID: sb.GetId(), Name: sb.GetName(), Namespace: sb.GetNamespace(), Workspace: sb.GetWorkspace(),
		State: "launching", CreatedAt: createdAt, Spec: sb.GetSpec(),
		LastStatus: sandboxStatus(sb.GetName(), "", "pending", createdAt),
	}
	if err := d.registry.Reserve(rec); err != nil {
		if errors.Is(err, safeguards.ErrCapacity) {
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		return nil, status.Error(codes.AlreadyExists, err.Error())
	}
	rollback := true
	defer func() {
		if rollback {
			d.registry.Rollback(sb.GetId())
		}
	}()

	logLevel := sb.GetSpec().GetLogLevel()
	if logLevel == "" {
		logLevel = "info"
	}
	userData, err := RenderUserData(UserDataParams{
		Transport: d.config.Transport, SandboxID: sb.GetId(), SandboxToken: sb.GetSpec().GetSandboxToken(),
		LogLevel: logLevel, MaxLifetime: d.registry.MaxLifetimeString(),
	})
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid bootstrap data: %v", err)
	}
	token := sha256.Sum256([]byte(d.config.GatewayID + "\x00" + sb.GetId() + "\x00" + sb.GetSpec().GetSandboxToken()))
	instance, err := d.provider.Launch(ctx, LaunchSpec{
		SandboxID: sb.GetId(), Name: sb.GetName(), Namespace: sb.GetNamespace(), Workspace: sb.GetWorkspace(),
		GatewayID: d.config.GatewayID, ImageID: d.config.AMIID, InstanceType: instanceType,
		SubnetID: d.config.SubnetID, SecurityGroupID: d.config.SecurityGroupID, KeyName: d.config.KeyName,
		UserData: userData, ClientToken: fmt.Sprintf("%x", token), UseSpot: d.config.UseSpot,
		DiskSizeGB: d.config.DiskSizeGB, CreatedAt: createdAt, MaxLifetime: d.registry.MaxLifetimeString(),
	})
	if err != nil {
		return nil, d.providerError(ctx, "launch", sb.GetId(), "", err)
	}
	if instance.State == "" {
		instance.State = "pending"
	}
	updated, ok := d.registry.Update(sb.GetId(), func(r *safeguards.Record) {
		r.InstanceID, r.State, r.CreatedAt = instance.ID, instance.State, instance.CreatedAt
		r.LastStatus = sandboxStatus(r.Name, instance.ID, instance.State, createdAt)
	})
	if !ok {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		terminateErr := d.provider.Terminate(cleanupCtx, []string{instance.ID})
		cancel()
		if terminateErr != nil {
			slog.Error("compensating termination failed", "sandbox_id", sb.GetId(), "instance_id", instance.ID, "error", terminateErr)
		}
		return nil, status.Error(codes.Internal, "launched instance could not be committed; cleanup requested")
	}
	rollback = false
	d.registry.BroadcastSandbox(updated)
	slog.Info("sandbox launch accepted", "sandbox_id", sb.GetId(), "instance_id", instance.ID, "instance_type", instanceType)
	return &computepb.CreateSandboxResponse{}, nil
}

func (d *EC2Driver) GetSandbox(ctx context.Context, req *computepb.GetSandboxRequest) (*computepb.GetSandboxResponse, error) {
	rec, err := d.resolve(ctx, req.GetSandboxId(), req.GetSandboxName())
	if err != nil {
		return nil, err
	}
	return &computepb.GetSandboxResponse{Sandbox: rec.ToDriverSandbox()}, nil
}

func (d *EC2Driver) ListSandboxes(ctx context.Context, _ *computepb.ListSandboxesRequest) (*computepb.ListSandboxesResponse, error) {
	instances, err := d.provider.List(ctx, InstanceFilter{GatewayID: d.config.GatewayID})
	if err != nil {
		return nil, d.providerError(ctx, "list", "", "", err)
	}
	response := &computepb.ListSandboxesResponse{Sandboxes: make([]*computepb.DriverSandbox, 0, len(instances))}
	for _, instance := range instances {
		if instance.State == "terminated" || instance.SandboxID == "" {
			continue
		}
		response.Sandboxes = append(response.Sandboxes, instanceToDriverSandbox(instance))
	}
	return response, nil
}

func (d *EC2Driver) StopSandbox(ctx context.Context, req *computepb.StopSandboxRequest) (*computepb.StopSandboxResponse, error) {
	rec, err := d.resolve(ctx, req.GetSandboxId(), req.GetSandboxName())
	if err != nil {
		return nil, err
	}
	if err := d.provider.Stop(ctx, rec.InstanceID); err != nil {
		return nil, d.providerError(ctx, "stop", rec.SandboxID, rec.InstanceID, err)
	}
	updated, _ := d.registry.Update(rec.SandboxID, func(r *safeguards.Record) {
		r.State = "stopping"
		r.LastStatus = sandboxStatus(r.Name, r.InstanceID, "stopping", time.Now().UTC())
	})
	d.registry.BroadcastSandbox(updated)
	slog.Info("sandbox stop requested", "sandbox_id", rec.SandboxID, "instance_id", rec.InstanceID)
	return &computepb.StopSandboxResponse{}, nil
}

func (d *EC2Driver) DeleteSandbox(ctx context.Context, req *computepb.DeleteSandboxRequest) (*computepb.DeleteSandboxResponse, error) {
	rec, err := d.resolve(ctx, req.GetSandboxId(), req.GetSandboxName())
	if status.Code(err) == codes.NotFound {
		return &computepb.DeleteSandboxResponse{Deleted: false}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := d.provider.Terminate(ctx, []string{rec.InstanceID}); err != nil {
		return nil, d.providerError(ctx, "terminate", rec.SandboxID, rec.InstanceID, err)
	}
	updated, _ := d.registry.Update(rec.SandboxID, func(r *safeguards.Record) {
		r.State = "shutting-down"
		r.LastStatus = sandboxStatus(r.Name, r.InstanceID, "shutting-down", time.Now().UTC())
	})
	d.registry.BroadcastSandbox(updated)
	slog.Info("sandbox termination requested", "sandbox_id", rec.SandboxID, "instance_id", rec.InstanceID)
	return &computepb.DeleteSandboxResponse{Deleted: true}, nil
}

func (d *EC2Driver) resolve(ctx context.Context, id, name string) (safeguards.Record, error) {
	if (id == "") == (name == "") {
		return safeguards.Record{}, status.Error(codes.InvalidArgument, "exactly one of sandbox_id or sandbox_name is required")
	}
	if id != "" {
		if rec, ok := d.registry.Get(id); ok && rec.InstanceID != "" {
			return rec, nil
		}
	} else {
		var matches []safeguards.Record
		for _, rec := range d.registry.All() {
			if rec.Name == name && rec.InstanceID != "" {
				matches = append(matches, rec)
			}
		}
		if len(matches) == 1 {
			return matches[0], nil
		}
		if len(matches) > 1 {
			return safeguards.Record{}, status.Errorf(codes.FailedPrecondition, "sandbox name %q is ambiguous", name)
		}
	}
	instances, err := d.provider.List(ctx, InstanceFilter{GatewayID: d.config.GatewayID, SandboxID: id, Name: name})
	if err != nil {
		return safeguards.Record{}, d.providerError(ctx, "resolve", id, "", err)
	}
	var active []Instance
	for _, instance := range instances {
		if instance.State != "terminated" {
			active = append(active, instance)
		}
	}
	if len(active) == 0 {
		return safeguards.Record{}, status.Error(codes.NotFound, "sandbox not found")
	}
	if len(active) > 1 {
		return safeguards.Record{}, status.Error(codes.FailedPrecondition, "multiple provider instances match sandbox")
	}
	rec := recordFromInstance(active[0])
	_ = d.registry.Add(&rec)
	return rec, nil
}

func (d *EC2Driver) WatchSandboxes(_ *computepb.WatchSandboxesRequest, stream computepb.ComputeDriver_WatchSandboxesServer) error {
	ch, snapshot := d.registry.Subscribe()
	defer d.registry.RemoveWatcher(ch)
	for _, rec := range snapshot {
		if err := stream.Send(sandboxEvent(rec)); err != nil {
			return err
		}
	}
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case event, ok := <-ch:
			if !ok {
				slog.Warn("disconnecting slow sandbox watcher for resynchronization", "watch_overflow_disconnects", d.registry.WatchOverflowDisconnects())
				return status.Error(codes.Aborted, "watcher fell behind; reconnect for a fresh snapshot")
			}
			if err := stream.Send(event); err != nil {
				return err
			}
		}
	}
}

func (d *EC2Driver) providerError(ctx context.Context, operation, sandboxID, instanceID string, err error) error {
	slog.Error("EC2 operation failed", "operation", operation, "sandbox_id", sandboxID, "instance_id", instanceID, "gateway_id", d.config.GatewayID, "region", d.config.Region, "error", err)
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return status.Error(codes.Canceled, "operation canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "provider operation timed out")
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := strings.ToLower(apiErr.ErrorCode())
		switch {
		case strings.Contains(code, "unauthor"), strings.Contains(code, "accessdenied"):
			return status.Error(codes.PermissionDenied, "AWS denied the EC2 operation")
		case strings.Contains(code, "throttl"), strings.Contains(code, "requestlimit"):
			return status.Error(codes.Unavailable, "AWS temporarily throttled the EC2 operation")
		case strings.Contains(code, "invalidparameter"), strings.Contains(code, "invalidami"), strings.Contains(code, "invalidsubnet"):
			return status.Error(codes.FailedPrecondition, "AWS infrastructure configuration rejected the operation")
		}
	}
	return status.Error(codes.Unavailable, "EC2 operation failed")
}

func sandboxStatus(name, instanceID, state string, transition time.Time) *computepb.DriverSandboxStatus {
	return &computepb.DriverSandboxStatus{SandboxName: name, InstanceId: instanceID, Conditions: ec2StateToConditionsAt(state, transition)}
}

func ec2StateToConditions(state string) []*computepb.DriverCondition {
	return ec2StateToConditionsAt(state, time.Now().UTC())
}

func ec2StateToConditionsAt(state string, transition time.Time) []*computepb.DriverCondition {
	statusValue, reason, message := "Unknown", "Unknown", "unknown EC2 state: "+state
	switch state {
	case "launching", "pending":
		statusValue, reason, message = "False", "Provisioning", "EC2 instance is pending"
	case "running":
		statusValue, reason, message = "True", "Running", "EC2 instance is running"
	case "stopping":
		statusValue, reason, message = "False", "Stopping", "EC2 instance is stopping"
	case "stopped":
		statusValue, reason, message = "False", "Stopped", "EC2 instance is stopped"
	case "shutting-down", "terminated":
		statusValue, reason, message = "False", "Terminating", "EC2 instance is terminating"
	}
	return []*computepb.DriverCondition{{Type: "Ready", Status: statusValue, Reason: reason, Message: message, LastTransitionTime: transition.Format(time.RFC3339)}}
}

func recordFromInstance(instance Instance) safeguards.Record {
	return safeguards.Record{
		SandboxID: instance.SandboxID, Name: instance.Name, Namespace: instance.Namespace, Workspace: instance.Workspace,
		InstanceID: instance.ID, State: instance.State, CreatedAt: instance.CreatedAt,
		LastStatus: sandboxStatus(instance.Name, instance.ID, instance.State, time.Now().UTC()),
	}
}

func instanceToDriverSandbox(instance Instance) *computepb.DriverSandbox {
	return recordFromInstance(instance).ToDriverSandbox()
}

func sandboxEvent(rec safeguards.Record) *computepb.WatchSandboxesEvent {
	return &computepb.WatchSandboxesEvent{Payload: &computepb.WatchSandboxesEvent_Sandbox{
		Sandbox: &computepb.WatchSandboxesSandboxEvent{Sandbox: rec.ToDriverSandbox()},
	}}
}
