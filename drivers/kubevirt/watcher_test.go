package kubevirt

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rsturla/openshell-drivers-contrib/pkg/safeguards"
)

func TestWatcherReconcilesNewInstances(t *testing.T) {
	provider := &mockProvider{listFn: func(context.Context, VMFilter) ([]VMInstance, error) {
		return []VMInstance{
			{VMName: "openshell-sb1", SandboxID: "sb1", Name: "sandbox-sb1", Namespace: "openshell", State: "Running", CreatedAt: time.Now()},
		}, nil
	}}
	driver := newTestDriver(t, provider)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := driver.Initialize(ctx); err != nil {
		t.Fatal(err)
	}

	rec, ok := driver.registry.Get("sb1")
	if !ok {
		t.Fatal("expected sandbox sb1 to be recovered")
	}
	if rec.State != "Running" {
		t.Fatalf("expected Running state, got %s", rec.State)
	}
}

func TestWatcherTerminatesDuplicates(t *testing.T) {
	now := time.Now()
	provider := &mockProvider{listFn: func(context.Context, VMFilter) ([]VMInstance, error) {
		return []VMInstance{
			{VMName: "openshell-dup-1", SandboxID: "sb-dup", Name: "sandbox-dup", State: "Running", CreatedAt: now},
			{VMName: "openshell-dup-2", SandboxID: "sb-dup", Name: "sandbox-dup", State: "Running", CreatedAt: now.Add(time.Second)},
		}, nil
	}}
	driver := newTestDriver(t, provider)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := driver.Initialize(ctx); err != nil {
		t.Fatal(err)
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.terminates) != 1 {
		t.Fatalf("expected one terminate call, got %d", len(provider.terminates))
	}
	if provider.terminates[0] != "openshell-dup-2" {
		t.Fatalf("expected newer duplicate to be terminated, got %v", provider.terminates[0])
	}
}

func TestWatcherPrefersRunningVMOverOlderTerminalVMIState(t *testing.T) {
	now := time.Now()
	provider := &mockProvider{listFn: func(context.Context, VMFilter) ([]VMInstance, error) {
		return []VMInstance{
			{VMName: "openshell-old", SandboxID: "sb-mixed", State: "Succeeded", CreatedAt: now.Add(-time.Minute)},
			{VMName: "openshell-running", SandboxID: "sb-mixed", State: "Running", CreatedAt: now},
		}, nil
	}}
	driver := newTestDriver(t, provider)
	if err := driver.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, ok := driver.registry.Get("sb-mixed")
	if !ok || rec.InstanceID != "openshell-running" {
		t.Fatalf("running VM was not retained: %+v", rec)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.terminates) != 1 || provider.terminates[0] != "openshell-old" {
		t.Fatalf("wrong duplicate terminated: %v", provider.terminates)
	}
}

func TestWatcherRetainsVMWhenVMIIsTerminal(t *testing.T) {
	provider := &mockProvider{listFn: func(context.Context, VMFilter) ([]VMInstance, error) {
		return []VMInstance{
			{VMName: "openshell-done", SandboxID: "sb-done", Name: "sandbox-done", State: "Succeeded", CreatedAt: time.Now()},
		}, nil
	}}
	driver := newTestDriver(t, provider)
	// Pre-register so it can be deleted.
	_ = driver.registry.Reserve(safeguards.Record{
		SandboxID: "sb-done", Name: "sandbox-done", InstanceID: "openshell-done",
		State: "Running", CreatedAt: time.Now(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := driver.Initialize(ctx); err != nil {
		t.Fatal(err)
	}

	if _, ok := driver.registry.Get("sb-done"); !ok {
		t.Fatal("terminal VMI phase must not remove the authoritative VM record")
	}
}

func TestWatcherDetectsMissingInstances(t *testing.T) {
	callCount := 0
	var mu sync.Mutex
	provider := &mockProvider{listFn: func(context.Context, VMFilter) ([]VMInstance, error) {
		mu.Lock()
		callCount++
		mu.Unlock()
		// Return nothing -- the registered instance is gone.
		return nil, nil
	}}
	driver := newTestDriver(t, provider)
	// Pre-register an instance that will be "missing" from provider.
	_ = driver.registry.Reserve(safeguards.Record{
		SandboxID: "sb-gone", Name: "sandbox-gone", InstanceID: "openshell-gone",
		State: "Running", CreatedAt: time.Now(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// Poll multiple times to trigger missing threshold.
	for range missingPollThreshold {
		if err := driver.poll(ctx); err != nil {
			t.Fatal(err)
		}
	}

	if _, ok := driver.registry.Get("sb-gone"); ok {
		t.Fatal("expected missing sandbox to be removed after threshold polls")
	}
}

func TestWatcherRetainsMissingVMUntilChildCleanupSucceeds(t *testing.T) {
	failCleanup := true
	provider := &mockProvider{terminateFn: func(context.Context, string) error {
		if failCleanup {
			return errors.New("Secret deletion failed")
		}
		return nil
	}}
	driver := newTestDriver(t, provider)
	_ = driver.registry.Add(&safeguards.Record{SandboxID: "orphan-child", InstanceID: "openshell-gone", State: "deleting", CreatedAt: time.Now()})
	for range missingPollThreshold {
		if err := driver.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := driver.registry.Get("orphan-child"); !ok {
		t.Fatal("record was released while child cleanup still failed")
	}
	failCleanup = false
	if err := driver.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := driver.registry.Get("orphan-child"); ok {
		t.Fatal("record retained after cleanup confirmation")
	}
}

func TestWatcherTerminatesExpiredInstances(t *testing.T) {
	now := time.Now()
	expired := now.Add(-9 * time.Hour) // MaxInstanceAge is 8h in testConfig
	provider := &mockProvider{listFn: func(context.Context, VMFilter) ([]VMInstance, error) {
		return []VMInstance{
			{VMName: "openshell-old", SandboxID: "sb-old", Name: "sandbox-old", State: "Running", CreatedAt: expired},
		}, nil
	}}
	driver := newTestDriver(t, provider)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := driver.Initialize(ctx); err != nil {
		t.Fatal(err)
	}

	// The expired instance should have been terminated.
	provider.mu.Lock()
	defer provider.mu.Unlock()
	found := false
	for _, name := range provider.terminates {
		if name == "openshell-old" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected expired instance to be terminated")
	}
}

func TestWatcherRetainsAndRetriesExpiredTerminationFailure(t *testing.T) {
	provider := &mockProvider{
		listFn: func(context.Context, VMFilter) ([]VMInstance, error) {
			return []VMInstance{{VMName: "openshell-old", SandboxID: "old", State: "Running", CreatedAt: time.Now().Add(-9 * time.Hour)}}, nil
		},
		terminateFn: func(context.Context, string) error { return errors.New("temporary") },
	}
	driver := newTestDriver(t, provider)
	for range 2 {
		if err := driver.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	rec, ok := driver.registry.Get("old")
	if !ok || rec.State != "Running" {
		t.Fatalf("expired cleanup state lost: %+v", rec)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.terminates) != 2 {
		t.Fatalf("termination not retried: %v", provider.terminates)
	}
}

func TestWatcherListFailureDoesNotIncrementMissingPolls(t *testing.T) {
	provider := &mockProvider{listFn: func(context.Context, VMFilter) ([]VMInstance, error) {
		return nil, errors.New("page two failed")
	}}
	driver := newTestDriver(t, provider)
	_ = driver.registry.Add(&safeguards.Record{SandboxID: "safe", InstanceID: "openshell-safe", State: "Running", CreatedAt: time.Now()})
	if err := driver.poll(context.Background()); err == nil {
		t.Fatal("expected list failure")
	}
	rec, ok := driver.registry.Get("safe")
	if !ok || rec.MissingPolls != 0 {
		t.Fatalf("partial observation changed missing count: %+v", rec)
	}
}

func TestWatcherStartStopLifecycle(t *testing.T) {
	provider := &mockProvider{listFn: func(context.Context, VMFilter) ([]VMInstance, error) {
		return nil, nil
	}}
	cfg := testConfig()
	cfg.PollInterval = 5 * time.Millisecond
	driver, err := NewKubeVirtDriver(cfg, provider)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- driver.StartWatcher(ctx) }()

	// Let the watcher run a few poll cycles.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watcher returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not stop within timeout")
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.lists) == 0 {
		t.Fatal("watcher never polled")
	}
}
