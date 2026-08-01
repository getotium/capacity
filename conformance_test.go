package capacity_test

import (
	"context"
	"testing"

	"github.com/getotium/capacity"
)

// RunConformance is the shared conformance suite for capacity.Provider implementations.
// newProvider must return a provider pre-configured to return at least one offer for
// model and instanceType when Quote is called (so Launch can follow). The Fake passes
// this under task go:unit; the AWS provider passes it under //go:build integration.
func RunConformance(t *testing.T, newProvider func(t *testing.T) capacity.Provider, model, instanceType string) {
	t.Helper()

	t.Run("Name", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		if n := p.Name(); n == "" {
			t.Error("Name() returned empty string")
		}
	})

	t.Run("Quote_returns_offers", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		offers, err := p.Quote(context.Background(), capacity.Requirement{
			Model:         model,
			InstanceTypes: []string{instanceType},
			Region:        "us-east-1",
		})
		if err != nil {
			t.Fatalf("Quote: %v", err)
		}
		if len(offers) == 0 {
			t.Fatal("Quote: expected at least one offer, got none")
		}
		o := offers[0]
		if o.Provider == "" {
			t.Error("Offer.Provider is empty")
		}
		if o.InstanceType == "" {
			t.Error("Offer.InstanceType is empty")
		}
		if o.PricePerHour <= 0 {
			t.Errorf("Offer.PricePerHour = %v, want > 0", o.PricePerHour)
		}
	})

	t.Run("Quote_unknown_instance_type", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		offers, err := p.Quote(context.Background(), capacity.Requirement{
			Model:         model,
			InstanceTypes: []string{"x9999.notreal"},
			Region:        "us-east-1",
		})
		// Not an error — just no results.
		if err != nil {
			t.Fatalf("Quote with unknown type: unexpected error: %v", err)
		}
		if len(offers) != 0 {
			t.Errorf("Quote with unknown type: expected 0 offers, got %d", len(offers))
		}
	})

	t.Run("Launch_Terminate", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		ctx := context.Background()

		offers, err := p.Quote(ctx, capacity.Requirement{
			Model:         model,
			InstanceTypes: []string{instanceType},
			Region:        "us-east-1",
		})
		if err != nil || len(offers) == 0 {
			t.Skipf("Quote precondition failed: %v (offers=%d)", err, len(offers))
		}

		const workerID = "conf-worker-1"
		h, err := p.Launch(ctx, workerID, offers[0], capacity.WorkerSpec{Model: model})
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if h.ID != workerID {
			t.Errorf("WorkerHandle.ID = %q, want the supplied %q", h.ID, workerID)
		}
		if h.InstanceID == "" {
			t.Error("WorkerHandle.InstanceID is empty (Terminate needs it)")
		}
		if h.Provider == "" {
			t.Error("WorkerHandle.Provider is empty")
		}
		if h.Spec.Model != model {
			t.Errorf("WorkerHandle.Spec.Model = %q, want %q", h.Spec.Model, model)
		}

		// A launched-and-live worker must appear in ListOwned (the orphan-reconcile input)
		// with a terminatable InstanceID, before we tear it down.
		owned, err := p.ListOwned(ctx)
		if err != nil {
			t.Fatalf("ListOwned: %v", err)
		}
		found := false
		for _, oi := range owned {
			if oi.InstanceID == h.InstanceID {
				found = true
				if oi.WorkerID != workerID {
					t.Errorf("ListOwned WorkerID = %q, want %q", oi.WorkerID, workerID)
				}
			}
		}
		if !found {
			t.Errorf("ListOwned did not include the launched instance %q; owned=%+v", h.InstanceID, owned)
		}

		if err := p.Terminate(ctx, h); err != nil {
			t.Fatalf("Terminate: %v", err)
		}
	})

	t.Run("Terminate_handle_without_instance", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		// A handle with no InstanceID cannot be terminated — every provider must reject it
		// rather than silently no-op.
		err := p.Terminate(context.Background(), capacity.WorkerHandle{ID: "ghost"})
		if err == nil {
			t.Error("Terminate of a handle with no instance id should return an error")
		}
	})
}
