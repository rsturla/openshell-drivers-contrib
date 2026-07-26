package ec2

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/smithy-go"
	"github.com/rsturla/openshell-drivers-contrib/gen/computepb"
	"github.com/rsturla/openshell-drivers-contrib/pkg/contracttest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type mockProvider struct {
	mu          sync.Mutex
	launchFn    func(context.Context, LaunchSpec) (Instance, error)
	listFn      func(context.Context, InstanceFilter) ([]Instance, error)
	stopFn      func(context.Context, string) error
	terminateFn func(context.Context, []string) error
	launches    []LaunchSpec
	lists       []InstanceFilter
	stops       []string
	terminates  [][]string
}

func (m *mockProvider) Launch(ctx context.Context, spec LaunchSpec) (Instance, error) {
	m.mu.Lock()
	m.launches = append(m.launches, spec)
	fn := m.launchFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, spec)
	}
	return Instance{ID: "i-test", SandboxID: spec.SandboxID, Name: spec.Name, Namespace: spec.Namespace, Workspace: spec.Workspace, State: "pending", CreatedAt: spec.CreatedAt}, nil
}
func (m *mockProvider) List(ctx context.Context, filter InstanceFilter) ([]Instance, error) {
	m.mu.Lock()
	m.lists = append(m.lists, filter)
	fn := m.listFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, filter)
	}
	return nil, nil
}
func (m *mockProvider) Stop(ctx context.Context, id string) error {
	m.mu.Lock()
	m.stops = append(m.stops, id)
	fn := m.stopFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, id)
	}
	return nil
}
func (m *mockProvider) Terminate(ctx context.Context, ids []string) error {
	m.mu.Lock()
	m.terminates = append(m.terminates, append([]string(nil), ids...))
	fn := m.terminateFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, ids)
	}
	return nil
}

func testConfig() Config {
	return Config{AMIID: "ami-test", DefaultInstance: "c7i.4xlarge", SubnetID: "subnet-test", SecurityGroupID: "sg-test", Transport: insecureTransport("http://gateway.example"), GatewayID: "gw-test", Region: "us-east-1", PollInterval: time.Second, DiskSizeGB: 100, MaxInstances: 10, MaxInstanceAge: 8 * time.Hour}
}

func testSandbox(id string) *computepb.DriverSandbox {
	return &computepb.DriverSandbox{Id: id, Name: "sandbox-" + id, Namespace: "default", Workspace: "workspace", Spec: &computepb.DriverSandboxSpec{LogLevel: "debug", SandboxToken: "token/+==", Template: &computepb.DriverSandboxTemplate{Image: DefaultImage}}}
}

func newTestDriver(t *testing.T, provider InstanceProvider) *EC2Driver {
	t.Helper()
	driver, err := NewEC2Driver(testConfig(), provider)
	if err != nil {
		t.Fatal(err)
	}
	return driver
}

func TestNewEC2DriverValidatesConfiguration(t *testing.T) {
	if _, err := NewEC2Driver(Config{}, &mockProvider{}); err == nil {
		t.Fatal("expected invalid configuration error")
	}
	if _, err := NewEC2Driver(testConfig(), nil); err == nil {
		t.Fatal("expected nil provider error")
	}
}

func TestDriverValidationContract(t *testing.T) {
	contracttest.RunValidation(t, newTestDriver(t, &mockProvider{}), testSandbox("contract"))
}

func TestCreateSandboxLaunchPolicyAndRedaction(t *testing.T) {
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
	if launch.ClientToken == "" || len(launch.ClientToken) != 64 || launch.Namespace != "default" || launch.DiskSizeGB != 100 {
		t.Fatalf("unexpected launch policy: %+v", launch)
	}
	response, err := driver.GetSandbox(context.Background(), &computepb.GetSandboxRequest{SandboxId: "sb-1"})
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
	provider := &mockProvider{launchFn: func(_ context.Context, spec LaunchSpec) (Instance, error) {
		time.Sleep(20 * time.Millisecond)
		return Instance{ID: "i-one", SandboxID: spec.SandboxID, State: "pending", CreatedAt: spec.CreatedAt}, nil
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
	provider := &mockProvider{launchFn: func(context.Context, LaunchSpec) (Instance, error) { return Instance{}, errors.New("temporary") }}
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
	provider := &mockProvider{launchFn: func(_ context.Context, spec LaunchSpec) (Instance, error) {
		close(launched)
		<-release
		return Instance{ID: "i-orphan-risk", SandboxID: spec.SandboxID, State: "pending", CreatedAt: spec.CreatedAt}, nil
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
	if len(provider.terminates) != 1 || provider.terminates[0][0] != "i-orphan-risk" {
		t.Fatalf("compensating termination missing: %v", provider.terminates)
	}
}

func TestProviderErrorClassificationIsSanitized(t *testing.T) {
	provider := &mockProvider{launchFn: func(context.Context, LaunchSpec) (Instance, error) {
		return Instance{}, &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "account 123456 secret detail"}
	}}
	driver := newTestDriver(t, provider)
	_, err := driver.CreateSandbox(context.Background(), &computepb.CreateSandboxRequest{Sandbox: testSandbox("denied")})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "123456") || strings.Contains(err.Error(), "secret detail") {
		t.Fatalf("provider details leaked to client: %v", err)
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

func TestResolveByNameAndIdempotentDelete(t *testing.T) {
	provider := &mockProvider{listFn: func(_ context.Context, filter InstanceFilter) ([]Instance, error) {
		if filter.Name == "found" {
			return []Instance{{ID: "i-found", SandboxID: "sb-found", Name: "found", State: "running", CreatedAt: time.Now()}}, nil
		}
		return nil, nil
	}}
	driver := newTestDriver(t, provider)
	if _, err := driver.StopSandbox(context.Background(), &computepb.StopSandboxRequest{SandboxName: "found"}); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	if len(provider.stops) != 1 || provider.stops[0] != "i-found" {
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

func TestEC2StateConditions(t *testing.T) {
	for state, reason := range map[string]string{"pending": "Provisioning", "running": "Running", "stopping": "Stopping", "stopped": "Stopped", "shutting-down": "Terminating", "terminated": "Terminating"} {
		if got := ec2StateToConditions(state)[0].GetReason(); got != reason {
			t.Errorf("%s: got %s want %s", state, got, reason)
		}
	}
}
