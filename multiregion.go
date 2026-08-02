package capacity

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// MultiRegionProvider presents a set of per-region Providers (one client per region) as a
// single Provider, so a caller can Quote capacity across every configured region and pick
// among the offers itself. It is region-agnostic: which regions exist is data, wired in at
// construction, never compiled in.
//
// Routing is by region: Quote fans out and tags nothing (each sub-provider's offers already
// carry their Region); Launch/Terminate route on the offer/handle Region; ListOwned
// aggregates and stamps each instance with the region it was found in so a later terminate
// can be addressed. All sub-providers share one provider Name (e.g. "aws-spot").
type MultiRegionProvider struct {
	name      string
	providers map[string]Provider // region -> provider
}

var _ Provider = (*MultiRegionProvider)(nil)

// NewMultiRegionProvider groups per-region providers under one name. byRegion must be
// non-empty and every value non-nil.
func NewMultiRegionProvider(name string, byRegion map[string]Provider) (*MultiRegionProvider, error) {
	if len(byRegion) == 0 {
		return nil, errors.New("capacity: MultiRegionProvider needs at least one region")
	}
	m := make(map[string]Provider, len(byRegion))
	for region, p := range byRegion {
		if p == nil {
			return nil, fmt.Errorf("capacity: nil provider for region %q", region)
		}
		m[region] = p
	}
	return &MultiRegionProvider{name: name, providers: m}, nil
}

// Regions returns the configured regions, sorted — for logging and the provisioner's
// candidate set.
func (m *MultiRegionProvider) Regions() []string {
	out := make([]string, 0, len(m.providers))
	for r := range m.providers {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// Name implements Provider.
func (m *MultiRegionProvider) Name() string { return m.name }

// Quote fans the requirement out to every region and concatenates the offers. A region
// that errors is skipped (logged by the caller via the returned error only when ALL
// regions fail) so one bad region can't blind the scheduler to the others.
func (m *MultiRegionProvider) Quote(ctx context.Context, req Requirement) ([]Offer, error) {
	var (
		all      []Offer
		lastErr  error
		failures int
	)
	for _, region := range m.Regions() { // deterministic order
		r := req
		r.Region = region
		offers, err := m.providers[region].Quote(ctx, r)
		if err != nil {
			failures++
			lastErr = fmt.Errorf("quote %s: %w", region, err)
			continue
		}
		all = append(all, offers...)
	}
	// Only surface an error when every region failed; a partial result is still useful.
	if len(all) == 0 && failures > 0 {
		return nil, lastErr
	}
	return all, nil
}

// Launch routes to the offer's region.
func (m *MultiRegionProvider) Launch(ctx context.Context, id string, offer Offer, spec WorkerSpec) (WorkerHandle, error) {
	p, err := m.providerFor(offer.Region)
	if err != nil {
		return WorkerHandle{}, err
	}
	return p.Launch(ctx, id, offer, spec)
}

// Terminate routes to the handle's offer region.
func (m *MultiRegionProvider) Terminate(ctx context.Context, h WorkerHandle) error {
	p, err := m.providerFor(h.Offer.Region)
	if err != nil {
		return err
	}
	return p.Terminate(ctx, h)
}

// ListOwned aggregates owned instances across regions, stamping each with the region it
// was found in (its sub-provider need not know its own region) so orphan reconciliation can
// route the terminate. A per-region error fails the whole call — orphan reconciliation must
// see a complete picture or none, never a partial one that could miss a cost leak.
func (m *MultiRegionProvider) ListOwned(ctx context.Context) ([]OwnedInstance, error) {
	var all []OwnedInstance
	for _, region := range m.Regions() {
		owned, err := m.providers[region].ListOwned(ctx)
		if err != nil {
			return nil, fmt.Errorf("list owned %s: %w", region, err)
		}
		for i := range owned {
			owned[i].Region = region
			all = append(all, owned[i])
		}
	}
	return all, nil
}

// AuditInstances implements InstanceAuditor by fanning the account-wide scan across every
// region's provider. A region provider that doesn't implement InstanceAuditor contributes
// nothing rather than failing the whole sweep (the auditor is best-effort visibility).
func (m *MultiRegionProvider) AuditInstances(ctx context.Context) ([]AuditedInstance, error) {
	var all []AuditedInstance
	for _, region := range m.Regions() {
		auditor, ok := m.providers[region].(InstanceAuditor)
		if !ok {
			continue
		}
		got, err := auditor.AuditInstances(ctx)
		if err != nil {
			return nil, fmt.Errorf("audit %s: %w", region, err)
		}
		for i := range got {
			if got[i].Region == "" {
				got[i].Region = region
			}
			all = append(all, got[i])
		}
	}
	return all, nil
}

// providerFor resolves the provider for a region. With exactly one region an empty/unknown
// region falls back to the sole provider (single-region deployments and tests that don't
// stamp a region still work); otherwise the region must match.
func (m *MultiRegionProvider) providerFor(region string) (Provider, error) {
	if p, ok := m.providers[region]; ok {
		return p, nil
	}
	if len(m.providers) == 1 {
		for _, p := range m.providers {
			return p, nil
		}
	}
	return nil, fmt.Errorf("capacity: no provider for region %q", region)
}
