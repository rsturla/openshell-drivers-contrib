// Package safeguards provides transport-neutral safety primitives for
// OpenShell compute drivers: capacity leases, immutable records, expiration
// selection, and loss-aware event fan-out.
package safeguards

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/rsturla/openshell-drivers-contrib/gen/computepb"
	"google.golang.org/protobuf/proto"
)

const (
	DefaultMaxInstances   = 10
	DefaultMaxInstanceAge = 8 * time.Hour
	watchBufferSize       = 64
)

var (
	ErrCapacity = errors.New("instance capacity exhausted")
	ErrExists   = errors.New("sandbox already exists")
)

type Limits struct {
	MaxInstances   int
	MaxInstanceAge time.Duration
}

func (l Limits) EffectiveMaxInstances() int {
	if l.MaxInstances > 0 {
		return l.MaxInstances
	}
	return DefaultMaxInstances
}

func (l Limits) EffectiveMaxAge() time.Duration {
	if l.MaxInstanceAge > 0 {
		return l.MaxInstanceAge
	}
	return DefaultMaxInstanceAge
}

// Record is the provider-independent state for one sandbox. Records returned
// by Registry are snapshots and may be safely read without holding a lock.
type Record struct {
	SandboxID    string
	Name         string
	Namespace    string
	Workspace    string
	InstanceID   string
	State        string
	CreatedAt    time.Time
	MissingPolls int
	Spec         *pb.DriverSandboxSpec
	LastStatus   *pb.DriverSandboxStatus
	// Lease identifies the reservation generation for this sandbox ID. It is
	// intentionally transport-internal and prevents delayed provider calls from
	// committing into a newer sandbox that reused the same ID.
	Lease uint64
}

func (r Record) ToDriverSandbox() *pb.DriverSandbox {
	return &pb.DriverSandbox{
		Id: r.SandboxID, Name: r.Name, Namespace: r.Namespace, Workspace: r.Workspace,
		Spec: cloneRedactedSpec(r.Spec), Status: cloneStatus(r.LastStatus),
	}
}

func cloneRedactedSpec(spec *pb.DriverSandboxSpec) *pb.DriverSandboxSpec {
	if spec == nil {
		return nil
	}
	cloned := proto.Clone(spec).(*pb.DriverSandboxSpec)
	cloned.SandboxToken = ""
	return cloned
}

func cloneStatus(status *pb.DriverSandboxStatus) *pb.DriverSandboxStatus {
	if status == nil {
		return nil
	}
	return proto.Clone(status).(*pb.DriverSandboxStatus)
}

func cloneRecord(rec Record) Record {
	rec.Spec = cloneRedactedSpec(rec.Spec)
	rec.LastStatus = cloneStatus(rec.LastStatus)
	return rec
}

type Registry struct {
	mu                       sync.RWMutex
	records                  map[string]Record
	limits                   Limits
	watchers                 map[chan *pb.WatchSandboxesEvent]struct{}
	watchOverflowDisconnects atomic.Uint64
	nextLease                atomic.Uint64
}

func NewRegistry(limits Limits) *Registry {
	return &Registry{
		records: make(map[string]Record), limits: limits,
		watchers: make(map[chan *pb.WatchSandboxesEvent]struct{}),
	}
}

// Reserve atomically checks capacity and duplicate identity, then creates a
// pending capacity lease. Call Rollback if provider launch fails.
func (r *Registry) Reserve(rec Record) error {
	_, err := r.ReserveWithLease(rec)
	return err
}

// ReserveWithLease reserves capacity and returns a generation token that must
// be presented when committing an asynchronous provider launch.
func (r *Registry) ReserveWithLease(rec Record) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.records[rec.SandboxID]; exists {
		return 0, fmt.Errorf("%w: %q", ErrExists, rec.SandboxID)
	}
	if len(r.records) >= r.limits.EffectiveMaxInstances() {
		return 0, fmt.Errorf("%w (%d/%d)", ErrCapacity, len(r.records), r.limits.EffectiveMaxInstances())
	}
	rec.Lease = r.nextLease.Add(1)
	r.records[rec.SandboxID] = cloneRecord(rec)
	return rec.Lease, nil
}

// Add inserts recovered/provider-observed state. Capacity is deliberately not
// enforced so every existing instance remains tracked even after configuration
// is lowered.
func (r *Registry) Add(rec *Record) error {
	if rec == nil {
		return errors.New("record is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.records[rec.SandboxID]; exists {
		return fmt.Errorf("%w: %q", ErrExists, rec.SandboxID)
	}
	stored := *rec
	if stored.Lease == 0 {
		stored.Lease = r.nextLease.Add(1)
	}
	r.records[rec.SandboxID] = cloneRecord(stored)
	return nil
}

func (r *Registry) Get(id string) (Record, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.records[id]
	return cloneRecord(rec), ok
}

func (r *Registry) Delete(id string) (Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.records[id]
	delete(r.records, id)
	return cloneRecord(rec), ok
}

func (r *Registry) Rollback(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.records[id]; ok && rec.InstanceID == "" {
		delete(r.records, id)
	}
}

func (r *Registry) Update(id string, fn func(*Record)) (Record, bool) {
	return r.UpdateIf(id, nil, fn)
}

// UpdateIf atomically applies fn only when predicate accepts the current
// record. A nil predicate accepts any existing record.
func (r *Registry) UpdateIf(id string, predicate func(Record) bool, fn func(*Record)) (Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.records[id]
	if !ok || (predicate != nil && !predicate(cloneRecord(rec))) {
		return Record{}, false
	}
	fn(&rec)
	rec = cloneRecord(rec)
	r.records[id] = rec
	return cloneRecord(rec), true
}

// DeleteIf atomically removes a record only when predicate accepts it.
func (r *Registry) DeleteIf(id string, predicate func(Record) bool) (Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.records[id]
	if !ok || (predicate != nil && !predicate(cloneRecord(rec))) {
		return Record{}, false
	}
	delete(r.records, id)
	return cloneRecord(rec), true
}

func (r *Registry) All() []Record {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Record, 0, len(r.records))
	for _, rec := range r.records {
		out = append(out, cloneRecord(rec))
	}
	return out
}

func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.records)
}

// Expired returns candidates but intentionally retains them until the provider
// confirms termination.
func (r *Registry) Expired(now time.Time) []Record {
	maxAge := r.limits.EffectiveMaxAge()
	r.mu.RLock()
	defer r.mu.RUnlock()
	var expired []Record
	for _, rec := range r.records {
		if !rec.CreatedAt.IsZero() && now.Sub(rec.CreatedAt) > maxAge && rec.State != "terminated" {
			expired = append(expired, cloneRecord(rec))
		}
	}
	return expired
}

// Subscribe atomically registers a watcher and captures the current snapshot.
func (r *Registry) Subscribe() (chan *pb.WatchSandboxesEvent, []Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := make(chan *pb.WatchSandboxesEvent, watchBufferSize)
	r.watchers[ch] = struct{}{}
	snapshot := make([]Record, 0, len(r.records))
	for _, rec := range r.records {
		snapshot = append(snapshot, cloneRecord(rec))
	}
	return ch, snapshot
}

func (r *Registry) AddWatcher() chan *pb.WatchSandboxesEvent {
	ch, _ := r.Subscribe()
	return ch
}

func (r *Registry) RemoveWatcher(ch chan *pb.WatchSandboxesEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.watchers[ch]; ok {
		delete(r.watchers, ch)
		close(ch)
	}
}

// Broadcast disconnects a slow watcher on overflow. The client must reconnect
// and receive a fresh snapshot; state changes are never silently discarded.
func (r *Registry) Broadcast(event *pb.WatchSandboxesEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ch := range r.watchers {
		select {
		case ch <- proto.Clone(event).(*pb.WatchSandboxesEvent):
		default:
			r.watchOverflowDisconnects.Add(1)
			close(ch)
			delete(r.watchers, ch)
		}
	}
}

// WatchOverflowDisconnects is a monotonic diagnostic counter suitable for
// exporting through the operator's metrics stack.
func (r *Registry) WatchOverflowDisconnects() uint64 { return r.watchOverflowDisconnects.Load() }

func (r *Registry) BroadcastSandbox(rec Record) {
	r.Broadcast(&pb.WatchSandboxesEvent{Payload: &pb.WatchSandboxesEvent_Sandbox{
		Sandbox: &pb.WatchSandboxesSandboxEvent{Sandbox: rec.ToDriverSandbox()},
	}})
}

func (r *Registry) BroadcastDeleted(id string) {
	r.Broadcast(&pb.WatchSandboxesEvent{Payload: &pb.WatchSandboxesEvent_Deleted{
		Deleted: &pb.WatchSandboxesDeletedEvent{SandboxId: id},
	}})
}

func (r *Registry) MaxLifetimeString() string { return r.limits.EffectiveMaxAge().String() }
