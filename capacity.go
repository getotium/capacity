// Package capacity abstracts where Otium's workers run. A Provider quotes live
// prices, launches and terminates workers, and reports interruptions. AWS Spot is the
// first real implementation; the interface anticipates GCP/Azure/Oracle preemptible,
// RunPod, Vast.ai, bare metal, and homelab providers. A fake provider backs tests and
// local end-to-end runs of the provisioning loop.
//
// The scheduler combines the model catalog (model -> required spec -> candidate
// instance types) with the pricing index (historical baseline) and a Provider's live
// Quote to decide whether, where, and how much to provision. Quote — not the index —
// is what we provision against and pay. See docs/architecture.md.
package capacity

import (
	"context"
	"time"
)

// Requirement describes the capacity a model needs: the candidate instance types
// (from the catalog) and where to look.
type Requirement struct {
	Model         string
	InstanceTypes []string
	Region        string
}

// Kind distinguishes interruptible spot capacity from stable on-demand.
type Kind string

const (
	KindSpot     Kind = "spot"
	KindOnDemand Kind = "on_demand"
)

// Offer is a live, quotable unit of capacity — what the scheduler actually provisions
// against and the price we actually pay.
type Offer struct {
	Provider     string
	InstanceType string
	Region       string
	Zone         string
	Kind         Kind
	PricePerHour float64
	Currency     string
}

// WorkerSpec describes the worker to launch onto an Offer.
type WorkerSpec struct {
	// Model is the model the worker should serve.
	Model string
	// Image is the AMI ID or image reference for the instance (e.g. "ami-0abc123"). Zero
	// means the provider uses its own default; the fake ignores this field.
	Image string
	// ImageVariant selects which AMI lineage the provider resolves when Image is empty: the
	// newest self-owned AMI tagged otium:ami=<ImageVariant>. Zero means the default "worker"
	// lineage. It lets one model require a different baked runtime stack (e.g. a nightly vLLM
	// for a brand-new architecture) without moving the stable worker AMI. Ignored when Image
	// is set (an explicit id wins) and by providers that don't resolve images (the fake).
	ImageVariant string
	// UserData is the cloud-init or bootstrap script payload (base64-encoded for AWS).
	// Zero means no user-data is passed; the fake ignores this field.
	UserData string
	// Env is a map of additional environment variables to inject into the worker process.
	// Zero or nil means no extra variables; the fake ignores this field.
	Env map[string]string
	// InstanceProfile is the IAM instance profile name or ARN granting the worker its
	// AWS permissions (e.g. "otium-worker-profile"). Zero means no profile is attached;
	// the fake ignores this field.
	InstanceProfile string
}

// WorkerHandle identifies a launched worker so it can be tracked and terminated.
type WorkerHandle struct {
	// ID is the caller-supplied worker id (passed to Launch). It is the worker's identity
	// across the registry, the per-worker token, and the lease/heartbeat path.
	ID string
	// InstanceID is the provider's own id for the underlying machine (e.g. the EC2 instance
	// id). It lets a worker be terminated or reconciled straight from durable state, with no
	// in-provider tracking. May equal ID for providers with no separate notion.
	InstanceID string
	Provider   string
	Offer      Offer
	Spec       WorkerSpec
	LaunchedAt time.Time
}

// OwnedInstance is the provider's own view of one Otium-launched machine, discovered
// from provider inventory (the ownership tag) rather than the durable registry. It is
// the input to orphan reconciliation: an owned instance whose WorkerID is not live in
// the fleet registry is a cost leak (a launch the control plane lost track of after a
// crash or a dropped Launch response) and gets terminated.
type OwnedInstance struct {
	WorkerID   string // the otium:worker-id tag (the registry/handle id), may be empty on a partial launch
	InstanceID string // the provider machine id, always present — enough to Terminate
	Model      string
	Region     string // the region it runs in — set by MultiRegionProvider so a terminate can be routed
	Provider   string // the cloud it runs on — set by MultiCloudProvider so a cross-cloud terminate can be routed
	LaunchedAt time.Time
}

// Provider is a source of disposable compute. Implementations must be safe for
// concurrent use. Liveness is no longer the provider's concern: the durable worker
// registry (pkg/fleet) and worker heartbeats own it (see docs/worker-registry.md), so a
// provider holds no per-worker state — Launch returns everything Terminate later needs.
type Provider interface {
	// Name identifies the provider, e.g. "aws-spot" or "fake".
	Name() string
	// Quote returns live offers able to satisfy the requirement, cheapest-relevant
	// first is not required — the scheduler scores them.
	Quote(ctx context.Context, req Requirement) ([]Offer, error)
	// Launch provisions a worker onto an offer under the caller-supplied id (used as the
	// instance's ownership tag and idempotency token) and returns a handle once it is
	// accepted (not necessarily ready). The handle carries the provider InstanceID.
	Launch(ctx context.Context, id string, offer Offer, spec WorkerSpec) (WorkerHandle, error)
	// Terminate tears a worker down, addressed by the handle's InstanceID.
	Terminate(ctx context.Context, h WorkerHandle) error
	// ListOwned returns every not-yet-terminated instance the provider knows this Otium
	// owns (by ownership tag). It is the safety net: the provisioner cross-references it
	// against the durable registry and terminates anything the control plane isn't
	// tracking, so no instance can bill forever after a state loss.
	ListOwned(ctx context.Context) ([]OwnedInstance, error)
}
