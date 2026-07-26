package kubevirt

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/rsturla/openshell-drivers-contrib/pkg/safeguards"
)

const missingPollThreshold = 3

// Initialize performs authoritative reconciliation before the server accepts
// mutating requests.
func (d *KubeVirtDriver) Initialize(ctx context.Context) error {
	if err := d.provider.CheckReady(ctx); err != nil {
		return fmt.Errorf("kubevirt prerequisites: %w", err)
	}
	return d.poll(ctx)
}

// StartWatcher runs reconciliation until cancellation and reports unexpected
// failure to the shared server supervisor.
func (d *KubeVirtDriver) StartWatcher(ctx context.Context) error {
	slog.Info("watcher started", "poll_interval", d.config.PollInterval, "gateway_id", d.config.GatewayID, "namespace", d.config.Namespace)
	consecutiveFailures := 0
	delay := d.config.PollInterval
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("watcher stopped")
			return nil
		case <-timer.C:
			timeout := d.config.PollInterval
			if timeout < 5*time.Second {
				timeout = 5 * time.Second
			}
			pollCtx, cancel := context.WithTimeout(ctx, timeout)
			err := d.poll(pollCtx)
			cancel()
			if err != nil {
				consecutiveFailures++
				delay = min(d.config.PollInterval*time.Duration(1<<min(consecutiveFailures, 6)), time.Minute)
				jitter := time.Duration(rand.Int64N(max(int64(delay/4), 1)))
				delay += jitter
				slog.Error("watcher poll failed", "gateway_id", d.config.GatewayID, "namespace", d.config.Namespace, "consecutive_failures", consecutiveFailures, "next_retry", delay, "error", err)
				timer.Reset(delay)
				continue
			}
			consecutiveFailures = 0
			delay = d.config.PollInterval
			timer.Reset(delay)
		}
	}
}

func (d *KubeVirtDriver) poll(ctx context.Context) error {
	instances, err := d.provider.List(ctx, VMFilter{GatewayID: d.config.GatewayID})
	if err != nil {
		return err
	}
	bySandbox := make(map[string][]VMInstance)
	for _, instance := range instances {
		if instance.SandboxID != "" {
			bySandbox[instance.SandboxID] = append(bySandbox[instance.SandboxID], instance)
		}
	}
	seen := make(map[string]struct{}, len(bySandbox))
	for sandboxID, matches := range bySandbox {
		sort.Slice(matches, func(i, j int) bool {
			if matches[i].CreatedAt.Equal(matches[j].CreatedAt) {
				return matches[i].VMName < matches[j].VMName
			}
			return matches[i].CreatedAt.Before(matches[j].CreatedAt)
		})
		live := matches[:0]
		for _, candidate := range matches {
			if !isTerminalState(candidate.State) {
				live = append(live, candidate)
			}
		}
		if len(live) == 0 {
			continue
		}
		keeper := 0
		for i, candidate := range live {
			if candidate.State != "Succeeded" && candidate.State != "Failed" {
				keeper = i
				break
			}
		}
		instance := live[keeper]
		seen[sandboxID] = struct{}{}
		if len(live) > 1 {
			duplicateNames := make([]string, 0, len(live)-1)
			for i, duplicate := range live {
				if i == keeper {
					continue
				}
				duplicateNames = append(duplicateNames, duplicate.VMName)
			}
			if len(duplicateNames) > 0 {
				for _, duplicateName := range duplicateNames {
					if err := d.provider.Terminate(ctx, duplicateName); err != nil {
						return fmt.Errorf("terminate duplicate VM %s for sandbox %s: %w", duplicateName, sandboxID, err)
					}
				}
				slog.Warn("duplicate sandbox VMs terminated", "sandbox_id", sandboxID, "kept_vm", instance.VMName, "terminated_vms", duplicateNames)
			}
		}
		if isTerminalState(instance.State) {
			if _, ok := d.registry.DeleteIf(sandboxID, func(current safeguards.Record) bool {
				return current.InstanceID == instance.VMName
			}); ok {
				d.registry.BroadcastDeleted(sandboxID)
				slog.Info("sandbox termination completed", "sandbox_id", sandboxID, "vm_name", instance.VMName)
			}
			continue
		}
		rec, ok := d.registry.Get(sandboxID)
		if !ok {
			recovered := recordFromInstance(instance)
			if recovered.CreatedAt.IsZero() || recovered.CreatedAt.After(time.Now().Add(5*time.Minute)) {
				slog.Error("managed VM has no trustworthy creation time", "sandbox_id", sandboxID, "vm_name", instance.VMName)
				recovered.CreatedAt = time.Now().Add(-d.config.MaxInstanceAge - time.Second)
			}
			if err := d.registry.Add(&recovered); err != nil {
				return fmt.Errorf("track recovered sandbox %s: %w", sandboxID, err)
			}
			d.registry.BroadcastSandbox(recovered)
			continue
		}
		observedState := instance.State
		if (rec.State == "deleting" || rec.State == "stopping") && observedState == "Running" {
			observedState = rec.State
		}
		if rec.State != observedState || rec.InstanceID != instance.VMName || rec.MissingPolls != 0 {
			transition := time.Now().UTC()
			updated, ok := d.registry.UpdateIf(sandboxID, func(current safeguards.Record) bool {
				return current.Lease == rec.Lease && (current.InstanceID == "" || current.InstanceID == rec.InstanceID)
			}, func(r *safeguards.Record) {
				r.InstanceID, r.MissingPolls = instance.VMName, 0
				if r.CreatedAt.IsZero() {
					r.CreatedAt = instance.CreatedAt
				}
				if r.State != observedState {
					r.State = observedState
					r.LastStatus = sandboxStatus(r.Name, r.InstanceID, observedState, transition)
				}
			})
			if ok && rec.State != observedState {
				d.registry.BroadcastSandbox(updated)
			}
		}
	}

	for _, rec := range d.registry.All() {
		if rec.InstanceID == "" {
			continue // in-flight capacity reservation
		}
		if _, ok := seen[rec.SandboxID]; ok {
			continue
		}
		updated, ok := d.registry.UpdateIf(rec.SandboxID, func(current safeguards.Record) bool {
			return current.Lease == rec.Lease && current.InstanceID == rec.InstanceID
		}, func(r *safeguards.Record) { r.MissingPolls++ })
		if ok && updated.MissingPolls >= missingPollThreshold {
			// A VM can disappear while a child Secret/DataVolume cleanup failed.
			// Idempotent termination must confirm child cleanup before capacity is released.
			if err := d.provider.Terminate(ctx, updated.InstanceID); err != nil {
				slog.Error("failed to confirm cleanup for absent VM", "sandbox_id", rec.SandboxID, "vm_name", rec.InstanceID, "error", err)
				continue
			}
			_, deleted := d.registry.DeleteIf(rec.SandboxID, func(current safeguards.Record) bool {
				return current.Lease == updated.Lease && current.InstanceID == updated.InstanceID && current.MissingPolls >= missingPollThreshold
			})
			if deleted {
				d.registry.BroadcastDeleted(rec.SandboxID)
			}
			slog.Warn("sandbox absent from consecutive complete polls", "sandbox_id", rec.SandboxID, "vm_name", rec.InstanceID, "missing_polls", updated.MissingPolls)
		}
	}

	for _, rec := range d.registry.Expired(time.Now()) {
		if rec.InstanceID == "" || rec.State == "deleting" {
			continue
		}
		if err := d.provider.Terminate(ctx, rec.InstanceID); err != nil {
			slog.Error("failed to terminate expired VM", "sandbox_id", rec.SandboxID, "vm_name", rec.InstanceID, "error", err)
			continue
		}
		updated, ok := d.registry.UpdateIf(rec.SandboxID, func(current safeguards.Record) bool {
			return current.Lease == rec.Lease && current.InstanceID == rec.InstanceID
		}, func(r *safeguards.Record) {
			r.State = "deleting"
			r.LastStatus = sandboxStatus(r.Name, r.InstanceID, "deleting", time.Now().UTC())
		})
		if ok {
			d.registry.BroadcastSandbox(updated)
		}
		slog.Warn("expired sandbox termination requested", "sandbox_id", rec.SandboxID, "vm_name", rec.InstanceID, "created_at", rec.CreatedAt, "max_age", d.config.MaxInstanceAge)
	}
	return nil
}
