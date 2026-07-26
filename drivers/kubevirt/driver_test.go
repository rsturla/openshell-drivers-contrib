package kubevirt

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsturla/openshell-drivers-contrib/gen/computepb"
	"github.com/rsturla/openshell-drivers-contrib/pkg/contracttest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	field "k8s.io/apimachinery/pkg/util/validation/field"
)

type mockProvider struct {
	mu          sync.Mutex
	launchFn    func(context.Context, VMSpec) (VMInstance, error)
	listFn      func(context.Context, VMFilter) ([]VMInstance, error)
	stopFn      func(context.Context, string) error
	terminateFn func(context.Context, string) error
	readyFn     func(context.Context) error
	launches    []VMSpec
	lists       []VMFilter
	stops       []string
	terminates  []string
}

func (m *mockProvider) CheckReady(ctx context.Context) error {
	if m.readyFn != nil {
		return m.readyFn(ctx)
	}
	return nil
}

func (m *mockProvider) Launch(ctx context.Context, spec VMSpec) (VMInstance, error) {
	m.mu.Lock()
	m.launches = append(m.launches, spec)
	fn := m.launchFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, spec)
	}
	return VMInstance{VMName: "openshell-" + spec.Name, SandboxID: spec.SandboxID, Name: spec.Name, Namespace: spec.SandboxNamespace, Workspace: spec.Workspace, State: "Pending", CreatedAt: spec.CreatedAt}, nil
}
func (m *mockProvider) List(ctx context.Context, filter VMFilter) ([]VMInstance, error) {
	m.mu.Lock()
	m.lists = append(m.lists, filter)
	fn := m.listFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, filter)
	}
	return nil, nil
}
func (m *mockProvider) Stop(ctx context.Context, name string) error {
	m.mu.Lock()
	m.stops = append(m.stops, name)
	fn := m.stopFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, name)
	}
	return nil
}
func (m *mockProvider) Terminate(ctx context.Context, name string) error {
	m.mu.Lock()
	m.terminates = append(m.terminates, name)
	fn := m.terminateFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, name)
	}
	return nil
}

func testSandbox(id string) *computepb.DriverSandbox {
	return &computepb.DriverSandbox{
		Id: id, Name: "sandbox-" + id, Namespace: "default", Workspace: "workspace",
		Spec: &computepb.DriverSandboxSpec{
			LogLevel: "debug", SandboxToken: "token/+==",
			Template: &computepb.DriverSandboxTemplate{Image: DefaultImage},
		},
	}
}

func newTestDriver(t *testing.T, provider VMProvider) *KubeVirtDriver {
	t.Helper()
	driver, err := NewKubeVirtDriver(testConfig(), provider)
	if err != nil {
		t.Fatal(err)
	}
	return driver
}

func TestNewKubeVirtDriverValidatesConfiguration(t *testing.T) {
	if _, err := NewKubeVirtDriver(Config{}, &mockProvider{}); err == nil {
		t.Fatal("expected invalid configuration error")
	}
	if _, err := NewKubeVirtDriver(testConfig(), nil); err == nil {
		t.Fatal("expected nil provider error")
	}
}

func TestDriverValidationContract(t *testing.T) {
	contracttest.RunValidation(t, newTestDriver(t, &mockProvider{}), testSandbox("contract"))
}

func TestCreateSandboxLaunchPolicy(t *testing.T) {
	provider := &mockProvider{}
	driver := newTestDriver(t, provider)
	sb := testSandbox("sb-1")
	if _, err := driver.CreateSandbox(context.Background(), &computepb.CreateSandboxRequest{Sandbox: sb}); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	if len(provider.launches) != 1 {
		t.Fatalf("launch calls=%d", len(provider.launches))
	}
	launch := provider.launches[0]
	provider.mu.Unlock()
	if launch.SandboxNamespace != "default" {
		t.Fatalf("expected sandbox metadata namespace 'default', got %q", launch.SandboxNamespace)
	}
	if launch.GatewayID != "gw-test" {
		t.Fatalf("expected gateway ID 'gw-test', got %q", launch.GatewayID)
	}
	if launch.BootSource != "fedora" {
		t.Fatalf("expected boot source 'fedora', got %q", launch.BootSource)
	}
	if launch.DiskSize != "100Gi" {
		t.Fatalf("expected disk size '100Gi', got %q", launch.DiskSize)
	}
	if launch.CloudInit == "" {
		t.Fatal("cloud-init data should not be empty")
	}
	if !strings.Contains(launch.CloudInit, "#cloud-config") {
		t.Fatal("cloud-init should contain #cloud-config header")
	}
}

func TestCreateSandboxTokenRedaction(t *testing.T) {
	provider := &mockProvider{}
	driver := newTestDriver(t, provider)
	sb := testSandbox("sb-redact")
	if _, err := driver.CreateSandbox(context.Background(), &computepb.CreateSandboxRequest{Sandbox: sb}); err != nil {
		t.Fatal(err)
	}
	response, err := driver.GetSandbox(context.Background(), &computepb.GetSandboxRequest{SandboxId: "sb-redact"})
	if err != nil {
		t.Fatal(err)
	}
	if token := response.GetSandbox().GetSpec().GetSandboxToken(); token != "" {
		t.Fatalf("token leaked from GetSandbox: %q", token)
	}
	if response.GetSandbox().GetNamespace() != "default" {
		t.Fatal("namespace was not preserved")
	}
}

func TestCreateSandboxConcurrentDuplicateLaunchesOnce(t *testing.T) {
	provider := &mockProvider{launchFn: func(_ context.Context, spec VMSpec) (VMInstance, error) {
		time.Sleep(20 * time.Millisecond)
		return VMInstance{VMName: "openshell-" + spec.Name, SandboxID: spec.SandboxID, Name: spec.Name, State: "Pending", CreatedAt: spec.CreatedAt}, nil
	}}
	driver := newTestDriver(t, provider)
	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := driver.CreateSandbox(context.Background(), &computepb.CreateSandboxRequest{Sandbox: testSandbox("same")})
			errs <- err
		}()
	}
	close(start)
	var success, duplicate int
	for range 2 {
		err := <-errs
		if err == nil {
			success++
		} else if status.Code(err) == codes.AlreadyExists {
			duplicate++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	provider.mu.Lock()
	launches := len(provider.launches)
	provider.mu.Unlock()
	if success != 1 || duplicate != 1 || launches != 1 {
		t.Fatalf("success=%d duplicate=%d launches=%d", success, duplicate, launches)
	}
}

func TestCreateFailureRollsBackReservation(t *testing.T) {
	provider := &mockProvider{launchFn: func(context.Context, VMSpec) (VMInstance, error) { return VMInstance{}, errors.New("temporary") }}
	driver := newTestDriver(t, provider)
	if _, err := driver.CreateSandbox(context.Background(), &computepb.CreateSandboxRequest{Sandbox: testSandbox("retry")}); status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable, got %v", err)
	}
	provider.launchFn = nil
	if _, err := driver.CreateSandbox(context.Background(), &computepb.CreateSandboxRequest{Sandbox: testSandbox("retry")}); err != nil {
		t.Fatalf("reservation was not rolled back: %v", err)
	}
}

func TestCreateCompensatesIfReservationDisappearsAfterLaunch(t *testing.T) {
	launched := make(chan struct{})
	release := make(chan struct{})
	provider := &mockProvider{launchFn: func(_ context.Context, spec VMSpec) (VMInstance, error) {
		close(launched)
		<-release
		return VMInstance{VMName: "openshell-orphan", SandboxID: spec.SandboxID, Name: spec.Name, State: "Pending", CreatedAt: spec.CreatedAt}, nil
	}}
	driver := newTestDriver(t, provider)
	done := make(chan error, 1)
	go func() {
		_, err := driver.CreateSandbox(context.Background(), &computepb.CreateSandboxRequest{Sandbox: testSandbox("commit-gap")})
		done <- err
	}()
	<-launched
	driver.registry.Delete("commit-gap")
	close(release)
	if err := <-done; status.Code(err) != codes.Internal {
		t.Fatalf("expected commit failure, got %v", err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.terminates) != 1 || provider.terminates[0] != "openshell-orphan" {
		t.Fatalf("compensating termination missing: %v", provider.terminates)
	}
}

func TestProviderErrorClassificationIsSanitized(t *testing.T) {
	provider := &mockProvider{launchFn: func(context.Context, VMSpec) (VMInstance, error) {
		return VMInstance{}, apierrors.NewForbidden(schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachines"}, "secret-name", errors.New("user 'system:serviceaccount:ns:sa' denied"))
	}}
	driver := newTestDriver(t, provider)
	_, err := driver.CreateSandbox(context.Background(), &computepb.CreateSandboxRequest{Sandbox: testSandbox("denied")})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "system:serviceaccount") {
		t.Fatalf("provider details leaked to client: %v", err)
	}
}

func TestProviderErrorClassificationUsesKubernetesReasons(t *testing.T) {
	resource := schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachines"}
	for name, tc := range map[string]struct {
		err  error
		code codes.Code
	}{
		"not found":      {apierrors.NewNotFound(resource, "private-vm-name"), codes.NotFound},
		"conflict":       {apierrors.NewConflict(resource, "private-vm-name", errors.New("resource version 123")), codes.Aborted},
		"too many":       {apierrors.NewTooManyRequests("private throttle detail", 1), codes.Unavailable},
		"server timeout": {apierrors.NewServerTimeout(resource, "list", 1), codes.Unavailable},
		"invalid":        {apierrors.NewInvalid(schema.GroupKind{Group: "kubevirt.io", Kind: "VirtualMachine"}, "private-vm-name", field.ErrorList{field.Invalid(field.NewPath("spec"), "secret", "private detail")}), codes.FailedPrecondition},
		"deadline":       {context.DeadlineExceeded, codes.DeadlineExceeded},
		"canceled":       {context.Canceled, codes.Canceled},
	} {
		t.Run(name, func(t *testing.T) {
			driver := newTestDriver(t, &mockProvider{})
			err := driver.providerError(context.Background(), "test", "sandbox", "vm", tc.err)
			if status.Code(err) != tc.code {
				t.Fatalf("got %v, want %s", err, tc.code)
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("provider details leaked: %v", err)
			}
		})
	}
}

func TestCreateValidationAndResources(t *testing.T) {
	driver := newTestDriver(t, &mockProvider{})
	bad := testSandbox("bad\nruncmd:")
	if _, err := driver.CreateSandbox(context.Background(), &computepb.CreateSandboxRequest{Sandbox: bad}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected injection rejection, got %v", err)
	}
	tooLarge := testSandbox("large")
	tooLarge.Spec.Template.Resources = &computepb.DriverResourceRequirements{CpuRequest: "9999"}
	if _, err := driver.CreateSandbox(context.Background(), &computepb.CreateSandboxRequest{Sandbox: tooLarge}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted, got %v", err)
	}
	gpu := testSandbox("gpu")
	count := uint32(1)
	gpu.Spec.ResourceRequirements = &computepb.ResourceRequirements{Gpu: &computepb.GpuResourceRequirements{Count: &count}}
	if _, err := driver.CreateSandbox(context.Background(), &computepb.CreateSandboxRequest{Sandbox: gpu}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected GPU rejection, got %v", err)
	}
}

func TestValidationRejectsUnsupportedExtensionFieldsAndBadQuantities(t *testing.T) {
	driver := newTestDriver(t, &mockProvider{})
	for name, mutate := range map[string]func(*computepb.DriverSandbox){
		"platform config": func(sb *computepb.DriverSandbox) { sb.Spec.Template.PlatformConfig = &structpb.Struct{} },
		"driver config":   func(sb *computepb.DriverSandbox) { sb.Spec.Template.DriverConfig = &structpb.Struct{} },
		"template labels": func(sb *computepb.DriverSandbox) { sb.Spec.Template.Labels = map[string]string{"unsafe": "value"} },
		"environment":     func(sb *computepb.DriverSandbox) { sb.Spec.Environment = map[string]string{"X": "Y"} },
		"invalid cpu": func(sb *computepb.DriverSandbox) {
			sb.Spec.Template.Resources = &computepb.DriverResourceRequirements{CpuRequest: "NaN"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			sandbox := testSandbox("reject-" + strings.ReplaceAll(name, " ", "-"))
			mutate(sandbox)
			_, err := driver.ValidateSandboxCreate(context.Background(), &computepb.ValidateSandboxCreateRequest{Sandbox: sandbox})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("expected InvalidArgument, got %v", err)
			}
		})
	}
}

func TestCreateRejectsMalformedProviderResultAndPreservesCapacity(t *testing.T) {
	provider := &mockProvider{launchFn: func(context.Context, VMSpec) (VMInstance, error) { return VMInstance{}, nil }}
	driver := newTestDriver(t, provider)
	if _, err := driver.CreateSandbox(context.Background(), &computepb.CreateSandboxRequest{Sandbox: testSandbox("malformed")}); status.Code(err) != codes.Unavailable {
		t.Fatalf("expected invalid provider result, got %v", err)
	}
	provider.launchFn = nil
	if _, err := driver.CreateSandbox(context.Background(), &computepb.CreateSandboxRequest{Sandbox: testSandbox("malformed")}); err != nil {
		t.Fatalf("malformed launch did not roll back capacity: %v", err)
	}
}

func TestDeleteDuringLaunchTombstonesAndCompensates(t *testing.T) {
	launched := make(chan struct{})
	release := make(chan struct{})
	provider := &mockProvider{launchFn: func(_ context.Context, spec VMSpec) (VMInstance, error) {
		close(launched)
		<-release
		return VMInstance{VMName: "openshell-racing", SandboxID: spec.SandboxID, Name: spec.Name, State: "Starting", CreatedAt: spec.CreatedAt}, nil
	}}
	driver := newTestDriver(t, provider)
	createDone := make(chan error, 1)
	go func() {
		_, err := driver.CreateSandbox(context.Background(), &computepb.CreateSandboxRequest{Sandbox: testSandbox("racing")})
		createDone <- err
	}()
	<-launched
	deleted, err := driver.DeleteSandbox(context.Background(), &computepb.DeleteSandboxRequest{SandboxId: "racing"})
	if err != nil || !deleted.GetDeleted() {
		t.Fatalf("delete did not tombstone launch: response=%v err=%v", deleted, err)
	}
	close(release)
	if err := <-createDone; status.Code(err) != codes.Internal {
		t.Fatalf("create was not rejected after tombstone: %v", err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.terminates) != 1 || provider.terminates[0] != "openshell-racing" {
		t.Fatalf("launch was not compensated: %v", provider.terminates)
	}
}

func TestWatcherObservationDuringLaunchDoesNotForceCompensation(t *testing.T) {
	launched := make(chan struct{})
	release := make(chan struct{})
	createdAt := time.Now().UTC()
	provider := &mockProvider{
		launchFn: func(_ context.Context, spec VMSpec) (VMInstance, error) {
			createdAt = spec.CreatedAt
			close(launched)
			<-release
			return VMInstance{VMName: "openshell-observed", SandboxID: spec.SandboxID, Name: spec.Name, State: "Starting", CreatedAt: spec.CreatedAt}, nil
		},
		listFn: func(context.Context, VMFilter) ([]VMInstance, error) {
			return []VMInstance{{VMName: "openshell-observed", SandboxID: "observed", Name: "sandbox-observed", State: "Starting", CreatedAt: createdAt}}, nil
		},
	}
	driver := newTestDriver(t, provider)
	done := make(chan error, 1)
	go func() {
		_, err := driver.CreateSandbox(context.Background(), &computepb.CreateSandboxRequest{Sandbox: testSandbox("observed")})
		done <- err
	}()
	<-launched
	if err := driver.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("watcher observation invalidated a healthy launch: %v", err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.terminates) != 0 {
		t.Fatalf("healthy launch was compensated: %v", provider.terminates)
	}
}

func TestResolveByNameAndIdempotentDelete(t *testing.T) {
	provider := &mockProvider{listFn: func(_ context.Context, filter VMFilter) ([]VMInstance, error) {
		if filter.Name == "found" {
			return []VMInstance{{VMName: "openshell-found", SandboxID: "sb-found", Name: "found", State: "Running", CreatedAt: time.Now()}}, nil
		}
		return nil, nil
	}}
	driver := newTestDriver(t, provider)
	if _, err := driver.StopSandbox(context.Background(), &computepb.StopSandboxRequest{SandboxName: "found"}); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	if len(provider.stops) != 1 || provider.stops[0] != "openshell-found" {
		t.Fatalf("unexpected stop calls: %v", provider.stops)
	}
	provider.mu.Unlock()
	response, err := driver.DeleteSandbox(context.Background(), &computepb.DeleteSandboxRequest{SandboxId: "missing"})
	if err != nil || response.GetDeleted() {
		t.Fatalf("idempotent delete response=%v err=%v", response, err)
	}
}

type mockWatchStream struct {
	ctx     context.Context
	mu      sync.Mutex
	events  []*computepb.WatchSandboxesEvent
	started chan struct{}
}

func (m *mockWatchStream) Send(event *computepb.WatchSandboxesEvent) error {
	m.mu.Lock()
	m.events = append(m.events, event)
	m.mu.Unlock()
	select {
	case <-m.started:
	default:
		close(m.started)
	}
	return nil
}
func (m *mockWatchStream) Context() context.Context   { return m.ctx }
func (*mockWatchStream) SetHeader(metadata.MD) error  { return nil }
func (*mockWatchStream) SendHeader(metadata.MD) error { return nil }
func (*mockWatchStream) SetTrailer(metadata.MD)       {}
func (*mockWatchStream) SendMsg(any) error            { return nil }
func (*mockWatchStream) RecvMsg(any) error            { return nil }

func TestWatchSnapshotDoesNotLeakTokenAndReceivesLiveEvent(t *testing.T) {
	driver := newTestDriver(t, &mockProvider{})
	if _, err := driver.CreateSandbox(context.Background(), &computepb.CreateSandboxRequest{Sandbox: testSandbox("watch")}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream := &mockWatchStream{ctx: ctx, started: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- driver.WatchSandboxes(&computepb.WatchSandboxesRequest{}, stream) }()
	select {
	case <-stream.started:
	case <-time.After(time.Second):
		t.Fatal("snapshot timeout")
	}
	stream.mu.Lock()
	if token := stream.events[0].GetSandbox().GetSandbox().GetSpec().GetSandboxToken(); token != "" {
		t.Fatalf("token leaked: %q", token)
	}
	stream.mu.Unlock()
	driver.registry.BroadcastDeleted("watch")
	time.Sleep(10 * time.Millisecond)
	cancel()
	<-done
	stream.mu.Lock()
	count := len(stream.events)
	stream.mu.Unlock()
	if count < 2 {
		t.Fatalf("live event was missed, count=%d", count)
	}
}

func TestVMIStateConditions(t *testing.T) {
	for state, reason := range map[string]string{
		"Pending":     "Provisioning",
		"Scheduling":  "Provisioning",
		"Scheduled":   "Provisioning",
		"Running":     "Running",
		"Succeeded":   "Completed",
		"Failed":      "Failed",
		"stopping":    "Stopping",
		"Stopping":    "Stopping",
		"deleting":    "Terminating",
		"Terminating": "Terminating",
	} {
		if got := vmiStateToConditions(state)[0].GetReason(); got != reason {
			t.Errorf("%s: got %s want %s", state, got, reason)
		}
	}
}

func TestIsTerminalState(t *testing.T) {
	for state, want := range map[string]bool{
		"Pending":   false,
		"Running":   false,
		"Succeeded": false,
		"Failed":    false,
		"Deleted":   true,
		"Unknown":   false,
		"stopping":  false,
		"deleting":  false,
		"launching": false,
	} {
		if got := isTerminalState(state); got != want {
			t.Errorf("isTerminalState(%q) = %v, want %v", state, got, want)
		}
	}
}
