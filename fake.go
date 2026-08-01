package capacity

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Fake is an in-process Provider for tests and local end-to-end runs of the
// provisioning loop. It quotes a configured set of offers, tracks launched and
// terminated workers, and — via an optional launch hook — can actually start an
// in-process worker so the full provision -> drain -> terminate loop runs without
// cloud. It is safe for concurrent use.
type Fake struct {
	mu       sync.Mutex
	name     string             // provider name reported by Name(); defaults to "fake"
	offers   map[string][]Offer // model -> offers; "" key is the fallback
	live     map[string]WorkerHandle
	launched int
	now      func() time.Time

	// OnLaunch, if set, is called after a worker is recorded as launched — the seam
	// that lets a local harness start an in-process worker bound to spec.Model.
	OnLaunch func(h WorkerHandle)
	// LaunchErr, if set, is consulted at the start of Launch: a non-nil return fails that launch
	// without recording it — the seam a test uses to simulate InsufficientInstanceCapacity for a
	// specific offer/AZ so the provisioner's cheapest-first offer fallback can be exercised.
	LaunchErr func(offer Offer) error
	// QuoteErr, if non-nil, is returned by Quote without quoting — the seam for exercising a
	// MultiCloudProvider fanning out over a cloud that's unreachable.
	QuoteErr error
	// ListOwnedErr, if non-nil, is returned by ListOwned without listing — the seam for the
	// fail-closed behaviour of aggregated orphan reconciliation.
	ListOwnedErr error
	// OnTerminate, if set, is called when a worker is terminated.
	OnTerminate func(h WorkerHandle)
}

var _ Provider = (*Fake)(nil)

// NewFake returns a fake provider. Register offers per model with SetOffers.
func NewFake() *Fake {
	return &Fake{
		name:   "fake",
		offers: make(map[string][]Offer),
		live:   make(map[string]WorkerHandle),
		now:    time.Now,
	}
}

// SetName overrides the provider name Name() reports. Tests use it to give sibling fakes
// distinct names so a MultiCloudProvider can route between them (its routing key is Name).
func (f *Fake) SetName(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.name = name
}

// SetClock overrides the time source (tests).
func (f *Fake) SetClock(now func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

// SetOffers registers the offers Quote returns for a model. An empty model registers
// the fallback used when a model has no specific offers.
func (f *Fake) SetOffers(model string, offers []Offer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offers[model] = offers
}

// Name implements Provider. Lock-free by design: SetName is setup-only, and Launch reads the
// field directly while already holding the lock (calling Name() there would self-deadlock).
func (f *Fake) Name() string { return f.name }

// Quote implements Provider. It returns the offers registered for req.Model (or the
// fallback), filtered to the requirement's candidate instance types if any are given.
func (f *Fake) Quote(_ context.Context, req Requirement) ([]Offer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.QuoteErr != nil {
		return nil, f.QuoteErr
	}

	offers, ok := f.offers[req.Model]
	if !ok {
		offers = f.offers[""]
	}
	if len(req.InstanceTypes) == 0 {
		return append([]Offer(nil), offers...), nil
	}
	want := make(map[string]bool, len(req.InstanceTypes))
	for _, it := range req.InstanceTypes {
		want[it] = true
	}
	var out []Offer
	for _, o := range offers {
		if want[o.InstanceType] {
			out = append(out, o)
		}
	}
	return out, nil
}

// Launch implements Provider. It uses the caller-supplied id as the handle id and
// synthesizes an InstanceID so Terminate (and a registry-reconstructed handle) works.
func (f *Fake) Launch(_ context.Context, id string, offer Offer, spec WorkerSpec) (WorkerHandle, error) {
	f.mu.Lock()
	if f.LaunchErr != nil {
		if err := f.LaunchErr(offer); err != nil {
			f.mu.Unlock()
			return WorkerHandle{}, err
		}
	}
	f.launched++
	h := WorkerHandle{
		ID:         id,
		InstanceID: "fake-instance-" + id,
		Provider:   f.name, // direct read: we already hold f.mu (Name() is lock-free anyway)
		Offer:      offer,
		Spec:       spec,
		LaunchedAt: f.now(),
	}
	f.live[h.ID] = h
	hook := f.OnLaunch
	f.mu.Unlock()

	if hook != nil {
		hook(h)
	}
	return h, nil
}

// Terminate implements Provider.
func (f *Fake) Terminate(_ context.Context, h WorkerHandle) error {
	f.mu.Lock()
	if _, ok := f.live[h.ID]; !ok {
		f.mu.Unlock()
		return fmt.Errorf("capacity: terminate unknown worker %q", h.ID)
	}
	delete(f.live, h.ID)
	hook := f.OnTerminate
	f.mu.Unlock()

	if hook != nil {
		hook(h)
	}
	return nil
}

// ListOwned implements Provider: the fake's currently-live handles as OwnedInstances,
// so orphan-reconciliation logic is exercisable without real cloud inventory.
func (f *Fake) ListOwned(_ context.Context) ([]OwnedInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ListOwnedErr != nil {
		return nil, f.ListOwnedErr
	}
	owned := make([]OwnedInstance, 0, len(f.live))
	for _, h := range f.live {
		owned = append(owned, OwnedInstance{
			WorkerID:   h.ID,
			InstanceID: h.InstanceID,
			Model:      h.Spec.Model,
			LaunchedAt: h.LaunchedAt,
		})
	}
	return owned, nil
}

// AddOwnedInstance injects an OwnedInstance that has NO matching live handle — an
// orphan, for testing reconciliation (an instance the provider knows but the registry
// doesn't). It is returned by ListOwned and removed by Terminate.
func (f *Fake) AddOwnedInstance(oi OwnedInstance) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live[oi.WorkerID] = WorkerHandle{ID: oi.WorkerID, InstanceID: oi.InstanceID, Provider: f.Name(), LaunchedAt: oi.LaunchedAt}
}

// LaunchedCount returns the total number of Launch calls (tests).
func (f *Fake) LaunchedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launched
}

// LiveCount returns the number of workers currently launched and not terminated.
func (f *Fake) LiveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.live)
}
