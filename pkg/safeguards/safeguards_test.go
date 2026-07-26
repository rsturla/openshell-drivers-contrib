package safeguards

import (
	"errors"
	"sync"
	"testing"
	"time"

	pb "github.com/rsturla/openshell-drivers-contrib/gen/computepb"
)

func TestReserveIsAtomic(t *testing.T) {
	r := NewRegistry(Limits{MaxInstances: 1, MaxInstanceAge: time.Hour})
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			results <- r.Reserve(Record{SandboxID: id})
		}(id)
	}
	close(start)
	wg.Wait()
	close(results)
	var successes, capacity int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrCapacity):
			capacity++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 || capacity != 1 || r.Count() != 1 {
		t.Fatalf("success=%d capacity=%d count=%d", successes, capacity, r.Count())
	}
}

func TestReserveRejectsDuplicateAndRollback(t *testing.T) {
	r := NewRegistry(Limits{MaxInstances: 2})
	if err := r.Reserve(Record{SandboxID: "same"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Reserve(Record{SandboxID: "same"}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists, got %v", err)
	}
	r.Rollback("same")
	if r.Count() != 0 {
		t.Fatal("pending reservation was not rolled back")
	}
}

func TestSnapshotsAreImmutableAndTokensRedacted(t *testing.T) {
	r := NewRegistry(Limits{})
	original := &Record{SandboxID: "sb", State: "pending", Spec: &pb.DriverSandboxSpec{SandboxToken: "secret", LogLevel: "debug"}}
	if err := r.Add(original); err != nil {
		t.Fatal(err)
	}
	original.State = "corrupted"
	original.Spec.LogLevel = "error"
	got, ok := r.Get("sb")
	if !ok || got.State != "pending" || got.Spec.GetLogLevel() != "debug" || got.Spec.GetSandboxToken() != "" {
		t.Fatalf("unexpected immutable snapshot: %+v", got)
	}
	got.State = "outside mutation"
	again, _ := r.Get("sb")
	if again.State != "pending" {
		t.Fatal("Get exposed internal record")
	}
	if token := again.ToDriverSandbox().GetSpec().GetSandboxToken(); token != "" {
		t.Fatalf("outbound token was not redacted: %q", token)
	}
}

func TestExpiredSelectsWithoutDeleting(t *testing.T) {
	r := NewRegistry(Limits{MaxInstanceAge: time.Hour})
	_ = r.Add(&Record{SandboxID: "old", InstanceID: "i-old", CreatedAt: time.Now().Add(-2 * time.Hour)})
	expired := r.Expired(time.Now())
	if len(expired) != 1 || expired[0].SandboxID != "old" {
		t.Fatalf("unexpected expired records: %+v", expired)
	}
	if _, ok := r.Get("old"); !ok {
		t.Fatal("expiration selection deleted cleanup state")
	}
}

func TestSubscribeSnapshotAndOverflow(t *testing.T) {
	r := NewRegistry(Limits{})
	_ = r.Add(&Record{SandboxID: "existing"})
	ch, snapshot := r.Subscribe()
	if len(snapshot) != 1 || snapshot[0].SandboxID != "existing" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	for i := 0; i < watchBufferSize+1; i++ {
		r.BroadcastDeleted("event")
	}
	for range ch {
	}
	if r.WatchOverflowDisconnects() != 1 {
		t.Fatalf("overflow counter=%d", r.WatchOverflowDisconnects())
	}
	// Closed channel proves a slow subscriber is forced to resnapshot instead
	// of silently missing an event while remaining connected.
}

func TestConcurrentGetUpdate(t *testing.T) {
	r := NewRegistry(Limits{})
	_ = r.Add(&Record{SandboxID: "sb"})
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = r.Get("sb") }()
		go func() {
			defer wg.Done()
			_, _ = r.Update("sb", func(rec *Record) { rec.State = "running" })
		}()
	}
	wg.Wait()
}

func TestLeasePreventsDelayedUpdateFromMutatingReusedSandboxID(t *testing.T) {
	r := NewRegistry(Limits{MaxInstances: 2})
	oldLease, err := r.ReserveWithLease(Record{SandboxID: "same", State: "launching"})
	if err != nil {
		t.Fatal(err)
	}
	r.Rollback("same")
	newLease, err := r.ReserveWithLease(Record{SandboxID: "same", State: "launching"})
	if err != nil {
		t.Fatal(err)
	}
	if oldLease == newLease {
		t.Fatal("reservation generation was reused")
	}
	if _, ok := r.UpdateIf("same", func(rec Record) bool { return rec.Lease == oldLease }, func(rec *Record) { rec.InstanceID = "old-vm" }); ok {
		t.Fatal("delayed old lease mutated the new reservation")
	}
	rec, ok := r.Get("same")
	if !ok || rec.InstanceID != "" || rec.Lease != newLease {
		t.Fatalf("new reservation corrupted: %+v", rec)
	}
}
