package capacity

import (
	"context"
	"time"
)

// OwnedImage is one machine image Otium baked (on AWS, an AMI tagged otium:ami), with the disk
// snapshots behind it. Images bill for their snapshots in every region they are copied to, for
// as long as they exist, so old ones are pruned rather than kept.
type OwnedImage struct {
	Provider  string    // the cloud provider name, for routing a delete
	Region    string    // the region holding this copy
	ImageID   string    // the provider image id (ami-…)
	Variant   string    // the image lineage (the otium:ami tag: "worker", or a variant)
	State     string    // the provider's state, e.g. "available" or "pending"
	CreatedAt time.Time // when this copy was created; zero if the provider did not say
	Snapshots []string  // the snapshots backing it, deleted with it
}

// OwnedSnapshot is one disk snapshot made for an Otium image (on AWS, tagged otium:ami). A
// snapshot no image refers to any more is left over from a delete that half failed or a bake
// that died before registering, and bills until someone deletes it.
type OwnedSnapshot struct {
	Provider   string
	Region     string
	SnapshotID string
	Variant    string    // the otium:ami tag it carries
	StartedAt  time.Time // when the snapshot was started; zero if the provider did not say
}

// ImagePruner lists and deletes Otium's own baked images and their snapshots. It is a separate,
// optional capability from Provider: a provider that does not bake images does not implement it.
//
// Listing is one call per region for images and one for snapshots, filtered to Otium's tag, so
// a sweep costs a handful of API calls however many images exist. A listing that fails in some
// regions returns what the others found together with the error: each region's images are
// pruned on their own, so a partial answer is safe to act on, unlike ListOwned's.
type ImagePruner interface {
	Name() string
	ListImages(ctx context.Context) ([]OwnedImage, error)
	ListImageSnapshots(ctx context.Context) ([]OwnedSnapshot, error)
	// DeleteImage deregisters the image, then deletes its snapshots. An image or snapshot that
	// is already gone counts as deleted, so two sweeps racing over one account both succeed.
	DeleteImage(ctx context.Context, img OwnedImage) error
	DeleteSnapshot(ctx context.Context, snap OwnedSnapshot) error
}
