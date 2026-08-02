package capacity

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// MultiCloudProvider presents providers from several clouds (each typically a
// MultiRegionProvider for one cloud) as a single Provider, so a caller can Quote the combined
// inventory of every configured cloud at once and pick among the offers itself. Which clouds
// exist is data, wired in at construction, never compiled in.
//
// Routing is by provider name: Quote fans out and concatenates (each sub-provider's offers
// already carry their own Provider); Launch routes on the offer's Provider; Terminate on the
// handle's Provider; ListOwned aggregates and stamps each instance with the provider it was
// found under so a later terminate can be addressed. Unlike MultiRegionProvider — whose
// per-region children deliberately SHARE one Name — the sub-providers here must have DISTINCT
// Names, because the Name is the routing key. Composition is expected: each sub-provider is
// typically itself a MultiRegionProvider, giving two-level routing (cloud -> region).
type MultiCloudProvider struct {
	name      string
	providers map[string]Provider // provider Name() -> provider
}

var _ Provider = (*MultiCloudProvider)(nil)

// NewMultiCloudProvider groups per-cloud providers under one aggregate name. subs must be
// non-empty, every provider non-nil, and their Names distinct and non-empty (the Name is the
// Launch/Terminate/ListOwned routing key).
func NewMultiCloudProvider(name string, subs ...Provider) (*MultiCloudProvider, error) {
	if len(subs) == 0 {
		return nil, errors.New("capacity: MultiCloudProvider needs at least one provider")
	}
	m := make(map[string]Provider, len(subs))
	for _, p := range subs {
		if p == nil {
			return nil, errors.New("capacity: nil sub-provider")
		}
		n := p.Name()
		if n == "" {
			return nil, errors.New("capacity: sub-provider has an empty Name (Names are the routing key)")
		}
		if _, dup := m[n]; dup {
			return nil, fmt.Errorf("capacity: duplicate sub-provider name %q (Names are the routing key)", n)
		}
		m[n] = p
	}
	return &MultiCloudProvider{name: name, providers: m}, nil
}

// Providers returns the configured sub-provider names, sorted — for logging and a
// deterministic fan-out order.
func (m *MultiCloudProvider) Providers() []string {
	out := make([]string, 0, len(m.providers))
	for n := range m.providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Name implements Provider.
func (m *MultiCloudProvider) Name() string { return m.name }

// Quote fans the requirement out to every cloud and concatenates the offers, each already
// stamped with its own Provider. A cloud that errors is skipped so one unreachable cloud
// can't blind the scheduler to the others; an error surfaces only when EVERY cloud fails.
func (m *MultiCloudProvider) Quote(ctx context.Context, req Requirement) ([]Offer, error) {
	var (
		all      []Offer
		lastErr  error
		failures int
	)
	for _, name := range m.Providers() { // deterministic order
		offers, err := m.providers[name].Quote(ctx, req)
		if err != nil {
			failures++
			lastErr = fmt.Errorf("quote %s: %w", name, err)
			continue
		}
		all = append(all, offers...)
	}
	// A partial result is still useful; only surface an error when every cloud failed.
	if len(all) == 0 && failures > 0 {
		return nil, lastErr
	}
	return all, nil
}

// Launch routes to the cloud that produced the offer.
func (m *MultiCloudProvider) Launch(ctx context.Context, id string, offer Offer, spec WorkerSpec) (WorkerHandle, error) {
	p, err := m.providerFor(offer.Provider)
	if err != nil {
		return WorkerHandle{}, err
	}
	return p.Launch(ctx, id, offer, spec)
}

// Terminate routes to the cloud named on the handle.
func (m *MultiCloudProvider) Terminate(ctx context.Context, h WorkerHandle) error {
	p, err := m.providerFor(h.Provider)
	if err != nil {
		return err
	}
	return p.Terminate(ctx, h)
}

// ListOwned aggregates owned instances across clouds, stamping each with the provider it was
// found under so orphan reconciliation can route the terminate (the sub-provider need not
// know its own cloud name). A per-cloud error fails the whole call — orphan reconciliation
// must see a complete picture or none, never a partial one that could miss a cost leak.
// Region is left exactly as the sub-provider set it (e.g. a MultiRegionProvider child).
func (m *MultiCloudProvider) ListOwned(ctx context.Context) ([]OwnedInstance, error) {
	var all []OwnedInstance
	for _, name := range m.Providers() {
		owned, err := m.providers[name].ListOwned(ctx)
		if err != nil {
			return nil, fmt.Errorf("list owned %s: %w", name, err)
		}
		for i := range owned {
			owned[i].Provider = name
			all = append(all, owned[i])
		}
	}
	return all, nil
}

// AuditInstances implements InstanceAuditor by fanning the account-wide scan across every cloud.
// A sub-provider that doesn't implement InstanceAuditor is skipped (that cloud contributes
// nothing rather than failing the sweep); Provider is stamped so a leak is attributable.
func (m *MultiCloudProvider) AuditInstances(ctx context.Context) ([]AuditedInstance, error) {
	var all []AuditedInstance
	for _, name := range m.Providers() {
		auditor, ok := m.providers[name].(InstanceAuditor)
		if !ok {
			continue
		}
		got, err := auditor.AuditInstances(ctx)
		if err != nil {
			return nil, fmt.Errorf("audit %s: %w", name, err)
		}
		for i := range got {
			if got[i].Provider == "" {
				got[i].Provider = name
			}
			all = append(all, got[i])
		}
	}
	return all, nil
}

// providerFor resolves the sub-provider by name. With exactly one cloud an empty/unknown name
// falls back to the sole provider (single-cloud wiring and tests that don't stamp a provider
// still work); otherwise the name must match one that was registered.
func (m *MultiCloudProvider) providerFor(name string) (Provider, error) {
	if p, ok := m.providers[name]; ok {
		return p, nil
	}
	if len(m.providers) == 1 {
		for _, p := range m.providers {
			return p, nil
		}
	}
	return nil, fmt.Errorf("capacity: no provider named %q", name)
}
