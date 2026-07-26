package kubevirt

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/rsturla/openshell-drivers-contrib/gen/computepb"
	"github.com/rsturla/openshell-drivers-contrib/pkg/quantity"
	"github.com/rsturla/openshell-drivers-contrib/pkg/safeguards"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	DriverName   = "openshell-driver-kubevirt"
	DefaultImage = "ghcr.io/nvidia/openshell/sandbox:0.0.91"

	LabelSandboxID   = "openshell.io/sandbox-id"
	LabelGatewayID   = "openshell.io/gateway-id"
	LabelWorkspace   = "openshell.io/workspace"
	AnnotCreatedAt   = "openshell.io/created-at"
	AnnotMaxLifetime = "openshell.io/max-lifetime"
)

// DriverVersion is overridden at build time with -ldflags.
var DriverVersion = "devel"

type KubeVirtDriver struct {
	computepb.UnimplementedComputeDriverServer
	config   Config
	provider VMProvider
	registry *safeguards.Registry
}

func NewKubeVirtDriver(config Config, provider VMProvider) (*KubeVirtDriver, error) {
	if provider == nil {
		return nil, errors.New("VM provider is required")
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid KubeVirt driver configuration: %w", err)
	}
	return &KubeVirtDriver{
		config: config, provider: provider,
		registry: safeguards.NewRegistry(safeguards.Limits{MaxInstances: config.MaxInstances, MaxInstanceAge: config.MaxInstanceAge}),
	}, nil
}

func (d *KubeVirtDriver) GetCapabilities(context.Context, *computepb.GetCapabilitiesRequest) (*computepb.GetCapabilitiesResponse, error) {
	return &computepb.GetCapabilitiesResponse{DriverName: DriverName, DriverVersion: DriverVersion, DefaultImage: DefaultImage}, nil
}

func (d *KubeVirtDriver) ValidateSandboxCreate(_ context.Context, req *computepb.ValidateSandboxCreateRequest) (*computepb.ValidateSandboxCreateResponse, error) {
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
		return errors.New("GPU resources are not supported by the KubeVirt driver")
	}
	resources := spec.GetTemplate().GetResources()
	if resources != nil {
		if err := quantity.ValidateRequirements(resources.GetCpuRequest(), resources.GetCpuLimit(), resources.GetMemoryRequest(), resources.GetMemoryLimit()); err != nil {
			return err
		}
	}
	if len(spec.GetEnvironment()) != 0 || len(spec.GetTemplate().GetEnvironment()) != 0 || len(spec.GetTemplate().GetLabels()) != 0 ||
		spec.GetTemplate().GetPlatformConfig() != nil || spec.GetTemplate().GetDriverConfig() != nil {
		return errors.New("sandbox environment, template labels, platform_config, and driver_config are not supported by the KubeVirt driver")
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

func (d *KubeVirtDriver) CreateSandbox(ctx context.Context, req *computepb.CreateSandboxRequest) (*computepb.CreateSandboxResponse, error) {
	sb := req.GetSandbox()
	if err := validateSandbox(sb); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	instanceType, err := SelectInstanceType(sb.GetSpec().GetTemplate().GetResources(), d.config.DefaultInstanceType)
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
		LastStatus: sandboxStatus(sb.GetName(), "", "Pending", createdAt),
	}
	lease, err := d.registry.ReserveWithLease(rec)
	if err != nil {
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
	cloudInit, err := RenderCloudInit(CloudInitParams{
		Transport: d.config.Transport, SandboxID: sb.GetId(), SandboxToken: sb.GetSpec().GetSandboxToken(),
		LogLevel: logLevel, MaxLifetime: d.registry.MaxLifetimeString(), ExpiresAt: createdAt.Add(d.config.MaxInstanceAge),
	})
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid bootstrap data: %v", err)
	}
	requestDigest := sha256.Sum256([]byte(strings.Join([]string{
		d.config.GatewayID, sb.GetId(), sb.GetName(), sb.GetNamespace(), sb.GetWorkspace(), instanceType,
		d.config.BootSource, d.config.BootSourceNamespace, d.config.DefaultPreference, d.config.StorageClass,
		d.config.DiskSize, cloudInit,
	}, "\x00")))
	resourceKey := sha256.Sum256([]byte(d.config.GatewayID + "\x00" + sb.GetId()))
	instance, err := d.provider.Launch(ctx, VMSpec{
		SandboxID: sb.GetId(), Name: sb.GetName(), SandboxNamespace: sb.GetNamespace(), Workspace: sb.GetWorkspace(),
		GatewayID: d.config.GatewayID, InstanceType: instanceType,
		BootSource: d.config.BootSource, BootSourceNamespace: d.config.BootSourceNamespace,
		Preference: d.config.DefaultPreference, StorageClass: d.config.StorageClass,
		CloudInit: cloudInit, ResourceKey: fmt.Sprintf("%x", resourceKey), RequestDigest: fmt.Sprintf("%x", requestDigest),
		DiskSize: d.config.DiskSize, StorageLabels: d.config.StorageLabels, StorageAnnotations: d.config.StorageAnnotations,
		CreatedAt: createdAt, MaxLifetime: d.registry.MaxLifetimeString(),
	})
	if err != nil {
		return nil, d.providerError(ctx, "launch", sb.GetId(), "", err)
	}
	if instance.State == "" {
		instance.State = "Pending"
	}
	if instance.VMName == "" {
		return nil, status.Error(codes.Unavailable, "KubeVirt launch returned an invalid VM identity")
	}
	if instance.CreatedAt.IsZero() {
		instance.CreatedAt = createdAt
	}
	updated, ok := d.registry.UpdateIf(sb.GetId(), func(r safeguards.Record) bool {
		return r.Lease == lease && r.State != "delete-requested" && r.State != "deleting" && r.State != "stopping"
	}, func(r *safeguards.Record) {
		r.InstanceID, r.State, r.CreatedAt = instance.VMName, instance.State, instance.CreatedAt
		r.LastStatus = sandboxStatus(r.Name, instance.VMName, instance.State, createdAt)
	})
	if !ok {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		terminateErr := d.provider.Terminate(cleanupCtx, instance.VMName)
		cancel()
		if terminateErr != nil {
			slog.Error("compensating termination failed", "sandbox_id", sb.GetId(), "vm_name", instance.VMName, "error", terminateErr)
		}
		return nil, status.Error(codes.Internal, "launched VM could not be committed; cleanup requested")
	}
	rollback = false
	d.registry.BroadcastSandbox(updated)
	slog.Info("sandbox launch accepted", "sandbox_id", sb.GetId(), "vm_name", instance.VMName, "instance_type", instanceType)
	return &computepb.CreateSandboxResponse{}, nil
}

func (d *KubeVirtDriver) GetSandbox(ctx context.Context, req *computepb.GetSandboxRequest) (*computepb.GetSandboxResponse, error) {
	rec, err := d.resolve(ctx, req.GetSandboxId(), req.GetSandboxName())
	if err != nil {
		return nil, err
	}
	return &computepb.GetSandboxResponse{Sandbox: rec.ToDriverSandbox()}, nil
}

func (d *KubeVirtDriver) ListSandboxes(ctx context.Context, _ *computepb.ListSandboxesRequest) (*computepb.ListSandboxesResponse, error) {
	instances, err := d.provider.List(ctx, VMFilter{GatewayID: d.config.GatewayID})
	if err != nil {
		return nil, d.providerError(ctx, "list", "", "", err)
	}
	response := &computepb.ListSandboxesResponse{Sandboxes: make([]*computepb.DriverSandbox, 0, len(instances))}
	for _, instance := range instances {
		if isTerminalState(instance.State) || instance.SandboxID == "" {
			continue
		}
		response.Sandboxes = append(response.Sandboxes, instanceToDriverSandbox(instance))
	}
	return response, nil
}

func (d *KubeVirtDriver) StopSandbox(ctx context.Context, req *computepb.StopSandboxRequest) (*computepb.StopSandboxResponse, error) {
	rec, err := d.resolve(ctx, req.GetSandboxId(), req.GetSandboxName())
	if err != nil {
		return nil, err
	}
	if err := d.provider.Stop(ctx, rec.InstanceID); err != nil {
		return nil, d.providerError(ctx, "stop", rec.SandboxID, rec.InstanceID, err)
	}
	updated, ok := d.registry.UpdateIf(rec.SandboxID, func(current safeguards.Record) bool {
		return current.InstanceID == rec.InstanceID
	}, func(r *safeguards.Record) {
		r.State = "stopping"
		r.LastStatus = sandboxStatus(r.Name, r.InstanceID, "stopping", time.Now().UTC())
	})
	if ok {
		d.registry.BroadcastSandbox(updated)
	}
	slog.Info("sandbox stop requested", "sandbox_id", rec.SandboxID, "vm_name", rec.InstanceID)
	return &computepb.StopSandboxResponse{}, nil
}

func (d *KubeVirtDriver) DeleteSandbox(ctx context.Context, req *computepb.DeleteSandboxRequest) (*computepb.DeleteSandboxResponse, error) {
	if d.markLaunchingDeletion(req.GetSandboxId(), req.GetSandboxName()) {
		return &computepb.DeleteSandboxResponse{Deleted: true}, nil
	}
	rec, err := d.resolve(ctx, req.GetSandboxId(), req.GetSandboxName())
	if status.Code(err) == codes.NotFound {
		return &computepb.DeleteSandboxResponse{Deleted: false}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := d.provider.Terminate(ctx, rec.InstanceID); err != nil {
		return nil, d.providerError(ctx, "terminate", rec.SandboxID, rec.InstanceID, err)
	}
	updated, ok := d.registry.UpdateIf(rec.SandboxID, func(current safeguards.Record) bool {
		return current.InstanceID == rec.InstanceID
	}, func(r *safeguards.Record) {
		r.State = "deleting"
		r.LastStatus = sandboxStatus(r.Name, r.InstanceID, "deleting", time.Now().UTC())
	})
	if ok {
		d.registry.BroadcastSandbox(updated)
	}
	slog.Info("sandbox termination requested", "sandbox_id", rec.SandboxID, "vm_name", rec.InstanceID)
	return &computepb.DeleteSandboxResponse{Deleted: true}, nil
}

func (d *KubeVirtDriver) markLaunchingDeletion(id, name string) bool {
	if (id == "") == (name == "") {
		return false
	}
	for _, rec := range d.registry.All() {
		if rec.InstanceID != "" || (id != "" && rec.SandboxID != id) || (name != "" && rec.Name != name) {
			continue
		}
		updated, ok := d.registry.UpdateIf(rec.SandboxID, func(current safeguards.Record) bool {
			return current.Lease == rec.Lease && current.InstanceID == "" && current.State == "launching"
		}, func(current *safeguards.Record) {
			current.State = "delete-requested"
			current.LastStatus = sandboxStatus(current.Name, "", "deleting", time.Now().UTC())
		})
		if ok {
			d.registry.BroadcastSandbox(updated)
			return true
		}
	}
	return false
}

func (d *KubeVirtDriver) resolve(ctx context.Context, id, name string) (safeguards.Record, error) {
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
	instances, err := d.provider.List(ctx, VMFilter{GatewayID: d.config.GatewayID, SandboxID: id, Name: name})
	if err != nil {
		return safeguards.Record{}, d.providerError(ctx, "resolve", id, "", err)
	}
	var active []VMInstance
	for _, instance := range instances {
		if !isTerminalState(instance.State) {
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
	if err := d.registry.Add(&rec); err == nil {
		return rec, nil
	}
	if updated, ok := d.registry.UpdateIf(rec.SandboxID, func(current safeguards.Record) bool {
		return current.InstanceID == "" && current.State == "launching"
	}, func(current *safeguards.Record) {
		current.InstanceID, current.State, current.CreatedAt = rec.InstanceID, rec.State, rec.CreatedAt
		current.LastStatus = rec.LastStatus
	}); ok {
		return updated, nil
	}
	if current, ok := d.registry.Get(rec.SandboxID); ok && current.InstanceID != "" {
		return current, nil
	}
	return safeguards.Record{}, status.Error(codes.Aborted, "sandbox lifecycle changed while resolving")
}

func (d *KubeVirtDriver) WatchSandboxes(_ *computepb.WatchSandboxesRequest, stream computepb.ComputeDriver_WatchSandboxesServer) error {
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

func (d *KubeVirtDriver) providerError(ctx context.Context, operation, sandboxID, vmName string, err error) error {
	slog.Error("KubeVirt operation failed", "operation", operation, "sandbox_id", sandboxID, "vm_name", vmName, "gateway_id", d.config.GatewayID, "namespace", d.config.Namespace, "provider_reason", apierrors.ReasonForError(err), "error_type", fmt.Sprintf("%T", err))
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return status.Error(codes.Canceled, "operation canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "provider operation timed out")
	}
	switch {
	case apierrors.IsUnauthorized(err), apierrors.IsForbidden(err):
		return status.Error(codes.PermissionDenied, "Kubernetes denied the KubeVirt operation")
	case apierrors.IsTooManyRequests(err), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), apierrors.IsServiceUnavailable(err):
		return status.Error(codes.Unavailable, "Kubernetes API temporarily throttled the operation")
	case apierrors.IsNotFound(err):
		return status.Error(codes.NotFound, "KubeVirt resource not found")
	case apierrors.IsAlreadyExists(err), apierrors.IsConflict(err):
		return status.Error(codes.Aborted, "KubeVirt resource changed concurrently")
	case apierrors.IsInvalid(err):
		return status.Error(codes.FailedPrecondition, "KubeVirt rejected the requested resource policy")
	}
	return status.Error(codes.Unavailable, "KubeVirt operation failed")
}

func sandboxStatus(name, vmName, state string, transition time.Time) *computepb.DriverSandboxStatus {
	return &computepb.DriverSandboxStatus{SandboxName: name, InstanceId: vmName, Conditions: vmiStateToConditionsAt(state, transition)}
}

func vmiStateToConditions(state string) []*computepb.DriverCondition {
	return vmiStateToConditionsAt(state, time.Now().UTC())
}

func vmiStateToConditionsAt(state string, transition time.Time) []*computepb.DriverCondition {
	statusValue, reason, message := "Unknown", "Unknown", "unknown VMI state: "+state
	switch state {
	case "launching", "Pending", "Provisioning", "Starting", "Scheduling", "Scheduled", "WaitingForVolumeBinding":
		statusValue, reason, message = "False", "Provisioning", "VMI is pending"
	case "Running":
		statusValue, reason, message = "True", "Running", "VMI is running"
	case "Succeeded":
		statusValue, reason, message = "False", "Completed", "VMI completed; VM resource remains authoritative"
	case "Failed":
		statusValue, reason, message = "False", "Failed", "VMI failed; VM resource remains authoritative"
	case "Stopped":
		statusValue, reason, message = "False", "Stopped", "VM is stopped"
	case "Paused":
		statusValue, reason, message = "False", "Paused", "VM is paused"
	case "Error", "CrashLoopBackOff", "ErrorUnschedulable", "ErrImagePull", "ImagePullBackOff", "ErrorPvcNotFound", "DataVolumeError":
		statusValue, reason, message = "False", "Failed", "VM cannot become ready"
	case "stopping", "Stopping":
		statusValue, reason, message = "False", "Stopping", "VMI is stopping"
	case "deleting", "Terminating":
		statusValue, reason, message = "False", "Terminating", "VMI is being deleted"
	case "Migrating", "WaitingForReceiver":
		statusValue, reason, message = "False", "Transitioning", "VM is transitioning between hosts"
	case "Unknown":
		statusValue, reason, message = "Unknown", "Unknown", "VMI state is unknown"
	}
	return []*computepb.DriverCondition{{Type: "Ready", Status: statusValue, Reason: reason, Message: message, LastTransitionTime: transition.Format(time.RFC3339)}}
}

func isTerminalState(state string) bool {
	return state == "Deleted"
}

func recordFromInstance(instance VMInstance) safeguards.Record {
	return safeguards.Record{
		SandboxID: instance.SandboxID, Name: instance.Name, Namespace: instance.Namespace, Workspace: instance.Workspace,
		InstanceID: instance.VMName, State: instance.State, CreatedAt: instance.CreatedAt,
		LastStatus: sandboxStatus(instance.Name, instance.VMName, instance.State, time.Now().UTC()),
	}
}

func instanceToDriverSandbox(instance VMInstance) *computepb.DriverSandbox {
	return recordFromInstance(instance).ToDriverSandbox()
}

func sandboxEvent(rec safeguards.Record) *computepb.WatchSandboxesEvent {
	return &computepb.WatchSandboxesEvent{Payload: &computepb.WatchSandboxesEvent_Sandbox{
		Sandbox: &computepb.WatchSandboxesSandboxEvent{Sandbox: rec.ToDriverSandbox()},
	}}
}
