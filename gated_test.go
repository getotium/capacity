package capacity

import (
	"context"
	"errors"
	"testing"
)

// stubGate answers a fixed verdict, recording what it was asked about.
type stubGate struct {
	allowed bool
	err     error
	asked   []string
}

func (g *stubGate) ProvisionAllowed(_ context.Context, provider string) (bool, error) {
	g.asked = append(g.asked, provider)
	return g.allowed, g.err
}

// recordingProvider is a Provider that records which methods reached it.
type recordingProvider struct {
	quoted, launched, terminated, listed bool
}

func (p *recordingProvider) Name() string { return "stub-spot" }
func (p *recordingProvider) Quote(context.Context, Requirement) ([]Offer, error) {
	p.quoted = true
	return []Offer{{Provider: "stub-spot", InstanceType: "t1", PricePerHour: 1}}, nil
}
func (p *recordingProvider) Launch(context.Context, string, Offer, WorkerSpec) (WorkerHandle, error) {
	p.launched = true
	return WorkerHandle{ID: "w1"}, nil
}
func (p *recordingProvider) Terminate(context.Context, WorkerHandle) error {
	p.terminated = true
	return nil
}
func (p *recordingProvider) ListOwned(context.Context) ([]OwnedInstance, error) {
	p.listed = true
	return nil, nil
}

func TestGatedProvider_AllowsWhenEnabled(t *testing.T) {
	t.Parallel()
	inner := &recordingProvider{}
	gate := &stubGate{allowed: true}
	p := NewGatedProvider("gcp", inner, gate, nil)

	offers, err := p.Quote(context.Background(), Requirement{InstanceTypes: []string{"t1"}})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if len(offers) != 1 || !inner.quoted {
		t.Fatalf("enabled cloud must reach the inner provider; offers=%v quoted=%v", offers, inner.quoted)
	}
	if _, err := p.Launch(context.Background(), "w1", Offer{}, WorkerSpec{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !inner.launched {
		t.Error("Launch did not reach the inner provider")
	}
	// The gate is asked about the registry key, not the offer name.
	for _, asked := range gate.asked {
		if asked != "gcp" {
			t.Errorf("gate asked about %q, want the registry key gcp (Name() is %q)", asked, inner.Name())
		}
	}
	if p.Name() != "stub-spot" {
		t.Errorf("Name = %q, want the wrapped provider's name (routing depends on it)", p.Name())
	}
}

func TestGatedProvider_DisabledQuotesNothingAndRefusesLaunch(t *testing.T) {
	t.Parallel()
	inner := &recordingProvider{}
	p := NewGatedProvider("gcp", inner, &stubGate{allowed: false}, nil)

	offers, err := p.Quote(context.Background(), Requirement{InstanceTypes: []string{"t1"}})
	if err != nil {
		t.Fatalf("Quote of a disabled cloud must not error: %v", err)
	}
	if len(offers) != 0 || inner.quoted {
		t.Fatalf("disabled cloud must not be quoted; offers=%v reached_inner=%v", offers, inner.quoted)
	}
	// Launch is an error, not a silent no-op: something already decided to spend money.
	if _, err := p.Launch(context.Background(), "w1", Offer{}, WorkerSpec{}); err == nil {
		t.Fatal("Launch on a disabled cloud must error")
	}
	if inner.launched {
		t.Error("Launch reached the inner provider despite the gate")
	}
}

// The safety property: disabling a cloud must never block cleaning up its instances, or an
// operator hitting the kill switch during an incident would leak billing.
func TestGatedProvider_TerminateAndListOwnedAreNeverGated(t *testing.T) {
	t.Parallel()
	for name, gate := range map[string]*stubGate{
		"disabled":   {allowed: false},
		"unreadable": {err: errors.New("db down")},
	} {
		inner := &recordingProvider{}
		p := NewGatedProvider("gcp", inner, gate, nil)
		if err := p.Terminate(context.Background(), WorkerHandle{ID: "w1", InstanceID: "i-1"}); err != nil {
			t.Errorf("%s: Terminate: %v", name, err)
		}
		if !inner.terminated {
			t.Errorf("%s: Terminate must always reach the cloud — a leaked instance bills forever", name)
		}
		if _, err := p.ListOwned(context.Background()); err != nil {
			t.Errorf("%s: ListOwned: %v", name, err)
		}
		if !inner.listed {
			t.Errorf("%s: ListOwned must always reach the cloud — the reaper needs it to find orphans", name)
		}
	}
}

// A kill switch that opens when the database hiccups is not a kill switch.
func TestGatedProvider_FailsClosedWhenGateUnreadable(t *testing.T) {
	t.Parallel()
	inner := &recordingProvider{}
	p := NewGatedProvider("gcp", inner, &stubGate{allowed: true, err: errors.New("db down")}, nil)

	offers, err := p.Quote(context.Background(), Requirement{InstanceTypes: []string{"t1"}})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if len(offers) != 0 || inner.quoted {
		t.Fatal("an unreadable gate must be treated as disabled, not as allowed")
	}
	if _, err := p.Launch(context.Background(), "w1", Offer{}, WorkerSpec{}); err == nil {
		t.Fatal("Launch must refuse when the gate cannot be read")
	}
}

// No gate wired must not change behaviour at all.
func TestNewGatedProvider_NilGateReturnsInner(t *testing.T) {
	t.Parallel()
	inner := &recordingProvider{}
	if got := NewGatedProvider("gcp", inner, nil, nil); got != Provider(inner) {
		t.Fatalf("nil gate must return the inner provider unwrapped, got %T", got)
	}
}
