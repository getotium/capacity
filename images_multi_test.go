package capacity

import (
	"context"
	"errors"
	"testing"
)

// imageFake is a Fake that also prunes images, with a canned listing.
type imageFake struct {
	*Fake
	images  []OwnedImage
	listErr error
	deleted []string
}

func (f *imageFake) ListImages(context.Context) ([]OwnedImage, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]OwnedImage(nil), f.images...), nil
}

func (f *imageFake) ListImageSnapshots(context.Context) ([]OwnedSnapshot, error) {
	return nil, f.listErr
}

func (f *imageFake) DeleteImage(_ context.Context, img OwnedImage) error {
	f.deleted = append(f.deleted, img.ImageID)
	return nil
}

func (f *imageFake) DeleteSnapshot(context.Context, OwnedSnapshot) error { return nil }

// One region failing to list does not hide the others' images, and a delete goes to the region
// the image is in.
func TestMultiRegionProvider_Images(t *testing.T) {
	t.Parallel()
	east := &imageFake{Fake: NewFake(), images: []OwnedImage{{ImageID: "ami-east"}}}
	west := &imageFake{Fake: NewFake(), listErr: errors.New("throttled")}
	m, err := NewMultiRegionProvider("aws-spot", map[string]Provider{"us-east-2": east, "eu-west-2": west})
	if err != nil {
		t.Fatal(err)
	}
	var ip ImagePruner = m

	got, err := ip.ListImages(context.Background())
	if err == nil {
		t.Fatal("a region's listing failure was not reported")
	}
	if len(got) != 1 || got[0].ImageID != "ami-east" || got[0].Region != "us-east-2" {
		t.Fatalf("ListImages = %+v, want ami-east stamped us-east-2", got)
	}
	if err := ip.DeleteImage(context.Background(), got[0]); err != nil {
		t.Fatal(err)
	}
	if len(east.deleted) != 1 || len(west.deleted) != 0 {
		t.Fatalf("delete routed wrong: east %v, west %v", east.deleted, west.deleted)
	}
	if err := ip.DeleteImage(context.Background(), OwnedImage{ImageID: "ami-x", Region: "ap-south-1"}); err == nil {
		t.Fatal("a delete in an unknown region succeeded")
	}
}

// A cloud that bakes no images is skipped; the one that does is listed and stamped.
func TestMultiCloudProvider_Images(t *testing.T) {
	t.Parallel()
	awsP := &imageFake{Fake: NewFake(), images: []OwnedImage{{ImageID: "ami-1", Region: "us-east-2"}}}
	awsP.SetName("aws-spot")
	gcp := NewFake()
	gcp.SetName("gcp-spot")
	mc, err := NewMultiCloudProvider("multi", awsP, gcp)
	if err != nil {
		t.Fatal(err)
	}
	got, err := mc.ListImages(context.Background())
	if err != nil || len(got) != 1 || got[0].Provider != "aws-spot" {
		t.Fatalf("ListImages = %+v, %v", got, err)
	}
	if err := mc.DeleteImage(context.Background(), got[0]); err != nil || len(awsP.deleted) != 1 {
		t.Fatalf("DeleteImage = %v, deleted %v", err, awsP.deleted)
	}
	if err := mc.DeleteImage(context.Background(), OwnedImage{Provider: "gcp-spot"}); err == nil {
		t.Fatal("a delete on a cloud that prunes no images succeeded")
	}
}
