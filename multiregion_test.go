package capacity

import (
	"context"
	"testing"
)

func TestMultiRegionProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	newMulti := func() (*MultiRegionProvider, *Fake, *Fake) {
		east := NewFake()
		west := NewFake()
		east.SetOffers("m", []Offer{{Provider: "fake", InstanceType: "g4dn.xlarge", Region: "us-east-2", Kind: KindSpot, PricePerHour: 0.20}})
		west.SetOffers("m", []Offer{{Provider: "fake", InstanceType: "g4dn.xlarge", Region: "us-west-2", Kind: KindSpot, PricePerHour: 0.15}})
		mp, err := NewMultiRegionProvider("fake", map[string]Provider{"us-east-2": east, "us-west-2": west})
		if err != nil {
			t.Fatalf("NewMultiRegionProvider: %v", err)
		}
		return mp, east, west
	}

	t.Run("quote aggregates offers across regions", func(t *testing.T) {
		t.Parallel()
		mp, _, _ := newMulti()
		offers, err := mp.Quote(ctx, Requirement{Model: "m", InstanceTypes: []string{"g4dn.xlarge"}})
		if err != nil {
			t.Fatalf("Quote: %v", err)
		}
		if len(offers) != 2 {
			t.Fatalf("got %d offers, want 2 (one per region)", len(offers))
		}
		regions := map[string]float64{}
		for _, o := range offers {
			regions[o.Region] = o.PricePerHour
		}
		if regions["us-east-2"] != 0.20 || regions["us-west-2"] != 0.15 {
			t.Fatalf("offers = %+v, want east 0.20 / west 0.15", regions)
		}
	})

	t.Run("launch routes to the offer's region", func(t *testing.T) {
		t.Parallel()
		mp, east, west := newMulti()
		// Launch onto the cheaper west offer.
		_, err := mp.Launch(ctx, "w1", Offer{Region: "us-west-2", InstanceType: "g4dn.xlarge"}, WorkerSpec{Model: "m"})
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if west.LiveCount() != 1 || east.LiveCount() != 0 {
			t.Fatalf("launch landed in wrong region: east=%d west=%d, want east=0 west=1", east.LiveCount(), west.LiveCount())
		}
	})

	t.Run("terminate routes by handle region", func(t *testing.T) {
		t.Parallel()
		mp, east, west := newMulti()
		h, _ := mp.Launch(ctx, "w1", Offer{Region: "us-east-2", InstanceType: "g4dn.xlarge"}, WorkerSpec{Model: "m"})
		if err := mp.Terminate(ctx, h); err != nil {
			t.Fatalf("Terminate: %v", err)
		}
		if east.LiveCount() != 0 {
			t.Fatalf("east live=%d after terminate, want 0", east.LiveCount())
		}
		_ = west
	})

	t.Run("list owned stamps the region", func(t *testing.T) {
		t.Parallel()
		mp, _, _ := newMulti()
		if _, err := mp.Launch(ctx, "we", Offer{Region: "us-east-2"}, WorkerSpec{Model: "m"}); err != nil {
			t.Fatal(err)
		}
		if _, err := mp.Launch(ctx, "ww", Offer{Region: "us-west-2"}, WorkerSpec{Model: "m"}); err != nil {
			t.Fatal(err)
		}
		owned, err := mp.ListOwned(ctx)
		if err != nil {
			t.Fatalf("ListOwned: %v", err)
		}
		if len(owned) != 2 {
			t.Fatalf("owned = %d, want 2", len(owned))
		}
		for _, oi := range owned {
			if oi.Region == "" {
				t.Fatalf("owned instance %q has no region stamped", oi.WorkerID)
			}
		}
	})

	t.Run("unknown region with multiple providers errors", func(t *testing.T) {
		t.Parallel()
		mp, _, _ := newMulti()
		if _, err := mp.Launch(ctx, "x", Offer{Region: "eu-west-1"}, WorkerSpec{Model: "m"}); err == nil {
			t.Fatal("expected error launching into an unconfigured region")
		}
	})

	t.Run("single region tolerates an empty offer region", func(t *testing.T) {
		t.Parallel()
		only := NewFake()
		mp, err := NewMultiRegionProvider("fake", map[string]Provider{"us-east-2": only})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mp.Launch(ctx, "w", Offer{}, WorkerSpec{Model: "m"}); err != nil {
			t.Fatalf("single-region launch with empty region should fall back: %v", err)
		}
	})
}
