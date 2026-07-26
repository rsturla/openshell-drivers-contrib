package ec2

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rsturla/openshell-drivers-contrib/pkg/safeguards"
)

func TestPollRecoversCreationTimeAndReapsExpired(t *testing.T) {
	created := time.Now().Add(-9 * time.Hour)
	provider := &mockProvider{listFn: func(context.Context, InstanceFilter) ([]Instance, error) {
		return []Instance{{ID: "i-old", SandboxID: "old", Name: "old", State: "running", CreatedAt: created}}, nil
	}}
	driver := newTestDriver(t, provider)
	if err := driver.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, ok := driver.registry.Get("old")
	if !ok || !rec.CreatedAt.Equal(created) || rec.State != "shutting-down" {
		t.Fatalf("unexpected recovered record: %+v", rec)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.terminates) != 1 || provider.terminates[0][0] != "i-old" {
		t.Fatalf("expired instance was not terminated: %v", provider.terminates)
	}
}

func TestPollRetainsExpiredRecordWhenTerminationFails(t *testing.T) {
	provider := &mockProvider{
		listFn: func(context.Context, InstanceFilter) ([]Instance, error) {
			return []Instance{{ID: "i-old", SandboxID: "old", State: "running", CreatedAt: time.Now().Add(-9 * time.Hour)}}, nil
		},
		terminateFn: func(context.Context, []string) error { return errors.New("throttled") },
	}
	driver := newTestDriver(t, provider)
	if err := driver.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec, ok := driver.registry.Get("old"); !ok || rec.State != "running" {
		t.Fatalf("cleanup state was lost: %+v %v", rec, ok)
	}
	if err := driver.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.terminates) != 2 {
		t.Fatalf("termination was not retried: %d", len(provider.terminates))
	}
}

func TestPollRequiresConsecutiveMissingSnapshots(t *testing.T) {
	provider := &mockProvider{}
	driver := newTestDriver(t, provider)
	_ = driver.registry.Add(&safeguards.Record{SandboxID: "missing", InstanceID: "i-missing", State: "running", CreatedAt: time.Now()})
	for i := 1; i < missingPollThreshold; i++ {
		if err := driver.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, ok := driver.registry.Get("missing"); !ok {
			t.Fatalf("record deleted after only %d misses", i)
		}
	}
	if err := driver.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := driver.registry.Get("missing"); ok {
		t.Fatal("record retained after threshold")
	}
}

func TestPollDoesNotApplyPartialFailure(t *testing.T) {
	provider := &mockProvider{listFn: func(context.Context, InstanceFilter) ([]Instance, error) { return nil, errors.New("page two failed") }}
	driver := newTestDriver(t, provider)
	_ = driver.registry.Add(&safeguards.Record{SandboxID: "safe", InstanceID: "i-safe", State: "running", CreatedAt: time.Now()})
	if err := driver.poll(context.Background()); err == nil {
		t.Fatal("expected list error")
	}
	rec, ok := driver.registry.Get("safe")
	if !ok || rec.MissingPolls != 0 {
		t.Fatalf("partial observation mutated state: %+v", rec)
	}
}

func TestPollTerminatesDuplicateInstances(t *testing.T) {
	now := time.Now()
	provider := &mockProvider{listFn: func(context.Context, InstanceFilter) ([]Instance, error) {
		return []Instance{
			{ID: "i-first", SandboxID: "duplicate", State: "running", CreatedAt: now.Add(-time.Minute)},
			{ID: "i-second", SandboxID: "duplicate", State: "running", CreatedAt: now},
		}, nil
	}}
	driver := newTestDriver(t, provider)
	if err := driver.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.terminates) != 1 || len(provider.terminates[0]) != 1 || provider.terminates[0][0] != "i-second" {
		t.Fatalf("unexpected duplicate cleanup: %v", provider.terminates)
	}
}

func TestStartWatcherStopsAndUsesPollTimeout(t *testing.T) {
	provider := &mockProvider{}
	cfg := testConfig()
	cfg.PollInterval = 5 * time.Millisecond
	driver, err := NewEC2Driver(cfg, provider)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- driver.StartWatcher(ctx) }()
	time.Sleep(15 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop")
	}
}
