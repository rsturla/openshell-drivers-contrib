package ec2

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/rsturla/openshell-drivers-contrib/pkg/safeguards"
)

const missingPollThreshold = 3

// Initialize performs authoritative reconciliation before the server accepts
// mutating requests.
func (d *EC2Driver) Initialize(ctx context.Context) error { return d.poll(ctx) }

// StartWatcher runs reconciliation until cancellation and reports unexpected
// failure to the shared server supervisor.
func (d *EC2Driver) StartWatcher(ctx context.Context) error {
	ticker := time.NewTicker(d.config.PollInterval)
	defer ticker.Stop()
	slog.Info("watcher started", "poll_interval", d.config.PollInterval, "gateway_id", d.config.GatewayID, "region", d.config.Region)
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			slog.Info("watcher stopped")
			return nil
		case <-ticker.C:
			timeout := d.config.PollInterval
			if timeout < 5*time.Second {
				timeout = 5 * time.Second
			}
			pollCtx, cancel := context.WithTimeout(ctx, timeout)
			err := d.poll(pollCtx)
			cancel()
			if err != nil {
				consecutiveFailures++
				slog.Error("watcher poll failed", "gateway_id", d.config.GatewayID, "region", d.config.Region, "consecutive_failures", consecutiveFailures, "next_retry", d.config.PollInterval, "error", err)
				continue
			}
			consecutiveFailures = 0
		}
	}
}

func (d *EC2Driver) poll(ctx context.Context) error {
	instances, err := d.provider.List(ctx, InstanceFilter{GatewayID: d.config.GatewayID})
	if err != nil {
		return err
	}
	bySandbox := make(map[string][]Instance)
	for _, instance := range instances {
		if instance.SandboxID != "" {
			bySandbox[instance.SandboxID] = append(bySandbox[instance.SandboxID], instance)
		}
	}
	seen := make(map[string]struct{}, len(bySandbox))
	for sandboxID, matches := range bySandbox {
		sort.Slice(matches, func(i, j int) bool {
			if matches[i].CreatedAt.Equal(matches[j].CreatedAt) {
				return matches[i].ID < matches[j].ID
			}
			return matches[i].CreatedAt.Before(matches[j].CreatedAt)
		})
		instance := matches[0]
		seen[sandboxID] = struct{}{}
		if len(matches) > 1 {
			duplicateIDs := make([]string, 0, len(matches)-1)
			for _, duplicate := range matches[1:] {
				if duplicate.State != "terminated" {
					duplicateIDs = append(duplicateIDs, duplicate.ID)
				}
			}
			if err := d.provider.Terminate(ctx, duplicateIDs); err != nil {
				return fmt.Errorf("terminate duplicate instances for sandbox %s: %w", sandboxID, err)
			}
			slog.Warn("duplicate sandbox instances terminated", "sandbox_id", sandboxID, "kept_instance_id", instance.ID, "terminated_instance_ids", duplicateIDs)
		}
		if instance.State == "terminated" {
			if _, ok := d.registry.Delete(sandboxID); ok {
				d.registry.BroadcastDeleted(sandboxID)
				slog.Info("sandbox termination completed", "sandbox_id", sandboxID, "instance_id", instance.ID)
			}
			continue
		}
		rec, ok := d.registry.Get(sandboxID)
		if !ok {
			recovered := recordFromInstance(instance)
			if recovered.CreatedAt.IsZero() || recovered.CreatedAt.After(time.Now().Add(5*time.Minute)) {
				slog.Error("managed instance has no trustworthy creation time", "sandbox_id", sandboxID, "instance_id", instance.ID)
				// Fail closed: an unknown-age instance is immediately eligible for cleanup.
				recovered.CreatedAt = time.Now().Add(-d.config.MaxInstanceAge - time.Second)
			}
			if err := d.registry.Add(&recovered); err != nil {
				return fmt.Errorf("track recovered sandbox %s: %w", sandboxID, err)
			}
			d.registry.BroadcastSandbox(recovered)
			continue
		}
		if rec.State != instance.State || rec.InstanceID != instance.ID || rec.MissingPolls != 0 {
			transition := time.Now().UTC()
			updated, _ := d.registry.Update(sandboxID, func(r *safeguards.Record) {
				r.InstanceID, r.MissingPolls = instance.ID, 0
				if r.CreatedAt.IsZero() {
					r.CreatedAt = instance.CreatedAt
				}
				if r.State != instance.State {
					r.State = instance.State
					r.LastStatus = sandboxStatus(r.Name, r.InstanceID, instance.State, transition)
				}
			})
			if rec.State != instance.State {
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
		updated, ok := d.registry.Update(rec.SandboxID, func(r *safeguards.Record) { r.MissingPolls++ })
		if ok && updated.MissingPolls >= missingPollThreshold {
			d.registry.Delete(rec.SandboxID)
			d.registry.BroadcastDeleted(rec.SandboxID)
			slog.Warn("sandbox absent from consecutive complete polls", "sandbox_id", rec.SandboxID, "instance_id", rec.InstanceID, "missing_polls", updated.MissingPolls)
		}
	}

	for _, rec := range d.registry.Expired(time.Now()) {
		if rec.InstanceID == "" || rec.State == "shutting-down" {
			continue
		}
		if err := d.provider.Terminate(ctx, []string{rec.InstanceID}); err != nil {
			// State remains retained, so the next complete poll retries.
			slog.Error("failed to terminate expired instance", "sandbox_id", rec.SandboxID, "instance_id", rec.InstanceID, "error", err)
			continue
		}
		updated, _ := d.registry.Update(rec.SandboxID, func(r *safeguards.Record) {
			r.State = "shutting-down"
			r.LastStatus = sandboxStatus(r.Name, r.InstanceID, "shutting-down", time.Now().UTC())
		})
		d.registry.BroadcastSandbox(updated)
		slog.Warn("expired sandbox termination requested", "sandbox_id", rec.SandboxID, "instance_id", rec.InstanceID, "created_at", rec.CreatedAt, "max_age", d.config.MaxInstanceAge)
	}
	return nil
}
