package capacity

import (
	"context"
	"fmt"
	"log/slog"
)

// Gate reports whether a cloud is currently cleared to launch workers. pricing.ProviderGate
// implements it over the provider registry.
type Gate interface {
	ProvisionAllowed(ctx context.Context, provider string) (bool, error)
}

// GatedProvider wraps one cloud's Provider with an operator kill switch, consulted on every
// Quote and Launch so flipping it takes effect within one provisioner tick rather than on the
// next restart — which is the whole point of having it during an incident.
//
// Terminate and ListOwned are deliberately NOT gated. Disabling a cloud must never block
// cleaning up its instances: that would leak billing at exactly the moment an operator is
// trying to stop it, and the reaper depends on both to find orphans.
//
// Wrap each cloud BEFORE combining them, so a MultiCloudProvider keeps shopping the others:
//
//	multi, _ := NewMultiCloudProvider("clouds",
//	    NewGatedProvider("aws", awsProvider, gate),
//	    NewGatedProvider("gcp", gcpProvider, gate))
type GatedProvider struct {
	inner Provider
	cloud string // registry key, e.g. "gcp" — NOT the offer name ("gcp-spot")
	gate  Gate
	log   *slog.Logger
}

var _ Provider = (*GatedProvider)(nil)

// NewGatedProvider wraps inner with the gate for cloud. A nil gate returns inner unwrapped, so
// callers with no registry wired are unaffected.
func NewGatedProvider(cloud string, inner Provider, gate Gate, log *slog.Logger) Provider {
	if gate == nil {
		return inner
	}
	if log == nil {
		log = slog.Default()
	}
	return &GatedProvider{inner: inner, cloud: cloud, gate: gate, log: log}
}

// Name implements Provider, returning the wrapped provider's name so offers, routing, and logs
// are unchanged by the wrapper.
func (g *GatedProvider) Name() string { return g.inner.Name() }

// Quote returns no offers when the cloud is gated off. No offers — rather than an error — is
// the honest answer: the cloud genuinely has no capacity available to us right now, and the
// score treats it exactly as it would a cloud that quoted nothing.
func (g *GatedProvider) Quote(ctx context.Context, req Requirement) ([]Offer, error) {
	allowed, err := g.allowed(ctx, "quote")
	if err != nil || !allowed {
		return nil, nil
	}
	return g.inner.Quote(ctx, req)
}

// Launch refuses when the cloud is gated off. Unlike Quote this is an error, not a silent
// no-op: something already decided to spend money here, and swallowing that would leave the
// provisioner believing a worker exists.
func (g *GatedProvider) Launch(ctx context.Context, id string, offer Offer, spec WorkerSpec) (WorkerHandle, error) {
	allowed, err := g.allowed(ctx, "launch")
	if err != nil {
		return WorkerHandle{}, fmt.Errorf("capacity: %s provision gate unreadable: %w", g.cloud, err)
	}
	if !allowed {
		return WorkerHandle{}, fmt.Errorf("capacity: %s is disabled for provisioning by an operator", g.cloud)
	}
	return g.inner.Launch(ctx, id, offer, spec)
}

// Terminate is never gated — see the type comment.
func (g *GatedProvider) Terminate(ctx context.Context, h WorkerHandle) error {
	return g.inner.Terminate(ctx, h)
}

// ListOwned is never gated — see the type comment.
func (g *GatedProvider) ListOwned(ctx context.Context) ([]OwnedInstance, error) {
	return g.inner.ListOwned(ctx)
}

// allowed reads the gate, failing CLOSED on error: a kill switch that opens when the database
// hiccups is not a kill switch, and pausing one tick of provisioning costs nothing.
func (g *GatedProvider) allowed(ctx context.Context, op string) (bool, error) {
	allowed, err := g.gate.ProvisionAllowed(ctx, g.cloud)
	if err != nil {
		g.log.ErrorContext(ctx, "capacity: provision gate unreadable; treating cloud as disabled",
			"cloud", g.cloud, "op", op, "err", err)
		return false, err
	}
	if !allowed {
		g.log.InfoContext(ctx, "capacity: cloud disabled for provisioning by an operator",
			"cloud", g.cloud, "op", op)
	}
	return allowed, nil
}
