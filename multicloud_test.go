package capacity

import (
	"context"
	"errors"
	"testing"
)

// newMultiCloudFakes returns two distinctly-named fakes ("aws-spot", "gcp-spot") wrapped in a
// MultiCloudProvider — the minimal cross-cloud fixture.
func newMultiCloudFakes(t *testing.T) (aws, gcp *Fake, mc *MultiCloudProvider) {
	t.Helper()
	aws = NewFake()
	aws.SetName("aws-spot")
	gcp = NewFake()
	gcp.SetName("gcp-spot")
	mc, err := NewMultiCloudProvider("multi", aws, gcp)
	if err != nil {
		t.Fatalf("NewMultiCloudProvider: %v", err)
	}
	return aws, gcp, mc
}

func mcOffer(provider, itype, region, zone string, price float64) Offer {
	return Offer{
		Provider: provider, InstanceType: itype, Region: region, Zone: zone,
		Kind: KindSpot, PricePerHour: price, Currency: "USD",
	}
}

func TestMultiCloudProvider_QuoteMergesAllClouds(t *testing.T) {
	t.Parallel()
	aws, gcp, mc := newMultiCloudFakes(t)
	aws.SetOffers("qwen", []Offer{
		mcOffer("aws-spot", "g4dn.xlarge", "us-east-2", "us-east-2a", 0.19),
	})
	gcp.SetOffers("qwen", []Offer{
		mcOffer("gcp-spot", "g2-standard-4", "us-central1", "us-central1-a", 0.22),
		mcOffer("gcp-spot", "n1-standard-4", "us-central1", "us-central1-b", 0.35),
	})

	offers, err := mc.Quote(context.Background(), Requirement{Model: "qwen"})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if len(offers) != 3 {
		t.Fatalf("want 3 merged offers, got %d: %+v", len(offers), offers)
	}
	byProv := map[string]int{}
	for _, o := range offers {
		byProv[o.Provider]++
	}
	if byProv["aws-spot"] != 1 || byProv["gcp-spot"] != 2 {
		t.Fatalf("offers not merged per cloud, keeping their own stamps: %v", byProv)
	}
}

func TestMultiCloudProvider_LaunchRoutesByOfferProvider(t *testing.T) {
	t.Parallel()
	aws, gcp, mc := newMultiCloudFakes(t)
	o := mcOffer("gcp-spot", "g2-standard-4", "us-central1", "us-central1-a", 0.22)

	h, err := mc.Launch(context.Background(), "w-1", o, WorkerSpec{Model: "qwen"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if h.Provider != "gcp-spot" {
		t.Fatalf("handle provider = %q, want gcp-spot", h.Provider)
	}
	if gcp.LiveCount() != 1 || aws.LiveCount() != 0 {
		t.Fatalf("launch routed to wrong cloud: aws=%d gcp=%d", aws.LiveCount(), gcp.LiveCount())
	}
}

func TestMultiCloudProvider_LaunchUnknownCloudErrors(t *testing.T) {
	t.Parallel()
	_, _, mc := newMultiCloudFakes(t)
	o := mcOffer("azure-spot", "Standard_NC4as_T4_v3", "eastus", "", 0.30)
	if _, err := mc.Launch(context.Background(), "w-1", o, WorkerSpec{}); err == nil {
		t.Fatal("want error launching an offer from an unregistered cloud")
	}
}

func TestMultiCloudProvider_TerminateRoutesByHandleProvider(t *testing.T) {
	t.Parallel()
	_, gcp, mc := newMultiCloudFakes(t)
	o := mcOffer("gcp-spot", "g2-standard-4", "us-central1", "us-central1-a", 0.22)
	h, err := mc.Launch(context.Background(), "w-1", o, WorkerSpec{Model: "qwen"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := mc.Terminate(context.Background(), h); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if gcp.LiveCount() != 0 {
		t.Fatalf("gcp still has %d live after terminate", gcp.LiveCount())
	}
}

func TestMultiCloudProvider_ListOwnedAggregatesAndStampsProvider(t *testing.T) {
	t.Parallel()
	aws, gcp, mc := newMultiCloudFakes(t)
	aws.AddOwnedInstance(OwnedInstance{WorkerID: "w-aws", InstanceID: "i-aws"})
	gcp.AddOwnedInstance(OwnedInstance{WorkerID: "w-gcp", InstanceID: "i-gcp"})

	owned, err := mc.ListOwned(context.Background())
	if err != nil {
		t.Fatalf("ListOwned: %v", err)
	}
	if len(owned) != 2 {
		t.Fatalf("want 2 owned across clouds, got %d", len(owned))
	}
	stamp := map[string]string{} // workerID -> provider stamp
	for _, oi := range owned {
		stamp[oi.WorkerID] = oi.Provider
	}
	if stamp["w-aws"] != "aws-spot" || stamp["w-gcp"] != "gcp-spot" {
		t.Fatalf("provider not stamped per cloud (needed to route an orphan terminate): %v", stamp)
	}
}

func TestMultiCloudProvider_QuoteSkipsUnreachableCloud(t *testing.T) {
	t.Parallel()
	aws, gcp, mc := newMultiCloudFakes(t)
	aws.QuoteErr = errors.New("aws unreachable")
	gcp.SetOffers("qwen", []Offer{mcOffer("gcp-spot", "g2-standard-4", "us-central1", "us-central1-a", 0.22)})

	offers, err := mc.Quote(context.Background(), Requirement{Model: "qwen"})
	if err != nil {
		t.Fatalf("one cloud down must not fail the whole quote: %v", err)
	}
	if len(offers) != 1 || offers[0].Provider != "gcp-spot" {
		t.Fatalf("want only the surviving cloud's offer, got %+v", offers)
	}
}

func TestMultiCloudProvider_QuoteErrorsOnlyWhenAllFail(t *testing.T) {
	t.Parallel()
	aws, gcp, mc := newMultiCloudFakes(t)
	aws.QuoteErr = errors.New("aws down")
	gcp.QuoteErr = errors.New("gcp down")
	if _, err := mc.Quote(context.Background(), Requirement{Model: "qwen"}); err == nil {
		t.Fatal("want error only-when every cloud fails to quote")
	}
}

func TestMultiCloudProvider_ListOwnedFailsClosed(t *testing.T) {
	t.Parallel()
	aws, gcp, mc := newMultiCloudFakes(t)
	aws.AddOwnedInstance(OwnedInstance{WorkerID: "w-aws", InstanceID: "i-aws"})
	gcp.ListOwnedErr = errors.New("gcp list failed")
	// A partial list could hide a cost leak on the failed cloud — reconciliation must get all
	// clouds or none, never a partial picture.
	if _, err := mc.ListOwned(context.Background()); err == nil {
		t.Fatal("want error when any cloud's ListOwned fails (fail-closed)")
	}
}

func TestNewMultiCloudProvider_Validation(t *testing.T) {
	t.Parallel()
	a := NewFake()
	a.SetName("aws-spot")
	dup := NewFake()
	dup.SetName("aws-spot")
	noName := NewFake()
	noName.SetName("")

	if _, err := NewMultiCloudProvider("multi"); err == nil {
		t.Error("want error with no sub-providers")
	}
	if _, err := NewMultiCloudProvider("multi", a, dup); err == nil {
		t.Error("want error on duplicate sub-provider names (the routing key)")
	}
	if _, err := NewMultiCloudProvider("multi", a, nil); err == nil {
		t.Error("want error on nil sub-provider")
	}
	if _, err := NewMultiCloudProvider("multi", noName); err == nil {
		t.Error("want error on empty sub-provider name")
	}
}
