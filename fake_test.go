package capacity

import (
	"context"
	"testing"
)

func offer(it string, price float64) Offer {
	return Offer{Provider: "fake", InstanceType: it, Region: "us-east-1", Zone: "us-east-1a", Kind: KindSpot, PricePerHour: price, Currency: "USD"}
}

func TestFake_QuoteByModelAndFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := NewFake()
	f.SetOffers("dummy", []Offer{offer("g4dn.xlarge", 0.16), offer("g5.xlarge", 0.30)})

	all, err := f.Quote(ctx, Requirement{Model: "dummy"})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d offers, want 2", len(all))
	}

	// Filter to candidate instance types.
	filtered, err := f.Quote(ctx, Requirement{Model: "dummy", InstanceTypes: []string{"g4dn.xlarge"}})
	if err != nil {
		t.Fatalf("Quote(filtered): %v", err)
	}
	if len(filtered) != 1 || filtered[0].InstanceType != "g4dn.xlarge" {
		t.Fatalf("filtered = %+v, want one g4dn.xlarge", filtered)
	}

	// Unknown model falls back to the "" offers (none here).
	none, err := f.Quote(ctx, Requirement{Model: "unknown"})
	if err != nil {
		t.Fatalf("Quote(unknown): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("unknown model got %d offers, want 0", len(none))
	}
}

func TestFake_LaunchTerminateTracking(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := NewFake()

	var launched []WorkerHandle
	f.OnLaunch = func(h WorkerHandle) { launched = append(launched, h) }

	h1, err := f.Launch(ctx, "w-1", offer("g5.xlarge", 0.30), WorkerSpec{Model: "dummy"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if h1.Spec.Model != "dummy" || h1.Offer.InstanceType != "g5.xlarge" {
		t.Fatalf("handle = %+v", h1)
	}
	if f.LiveCount() != 1 || f.LaunchedCount() != 1 {
		t.Fatalf("live=%d launched=%d, want 1/1", f.LiveCount(), f.LaunchedCount())
	}
	if len(launched) != 1 {
		t.Fatalf("OnLaunch called %d times, want 1", len(launched))
	}

	if err := f.Terminate(ctx, h1); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if f.LiveCount() != 0 {
		t.Fatalf("live=%d after terminate, want 0", f.LiveCount())
	}
	// Launched count is cumulative.
	if f.LaunchedCount() != 1 {
		t.Fatalf("launched=%d, want 1 (cumulative)", f.LaunchedCount())
	}

	// Terminating an unknown handle errors.
	if err := f.Terminate(ctx, WorkerHandle{ID: "nope"}); err == nil {
		t.Fatal("expected error terminating unknown worker")
	}
}
