package aws

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/getotium/capacity"
)

func (f *fakeEC2) DescribeSnapshots(_ context.Context, _ *ec2.DescribeSnapshotsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error) {
	return &ec2.DescribeSnapshotsOutput{Snapshots: f.snapshots}, nil
}

func (f *fakeEC2) DeregisterImage(_ context.Context, in *ec2.DeregisterImageInput, _ ...func(*ec2.Options)) (*ec2.DeregisterImageOutput, error) {
	if f.deregErr != nil {
		return nil, f.deregErr
	}
	f.deregistered = append(f.deregistered, aws.ToString(in.ImageId))
	return &ec2.DeregisterImageOutput{}, nil
}

func (f *fakeEC2) DeleteSnapshot(_ context.Context, in *ec2.DeleteSnapshotInput, _ ...func(*ec2.Options)) (*ec2.DeleteSnapshotOutput, error) {
	id := aws.ToString(in.SnapshotId)
	if err := f.deleteSnapErr[id]; err != nil {
		return nil, err
	}
	f.deletedSnaps = append(f.deletedSnaps, id)
	return &ec2.DeleteSnapshotOutput{}, nil
}

func amiTag(v string) []ec2types.Tag {
	return []ec2types.Tag{{Key: aws.String(tagAMI), Value: aws.String(v)}}
}

func TestListImages_MapsVariantStateAndSnapshots(t *testing.T) {
	t.Parallel()
	f := &fakeEC2{images: []ec2types.Image{{
		ImageId:      aws.String("ami-1"),
		CreationDate: aws.String("2026-10-21T12:30:00.000Z"),
		State:        ec2types.ImageStateAvailable,
		Tags:         amiTag("worker"),
		BlockDeviceMappings: []ec2types.BlockDeviceMapping{
			{Ebs: &ec2types.EbsBlockDevice{SnapshotId: aws.String("snap-1")}},
			{DeviceName: aws.String("/dev/sdb")}, // ephemeral: no snapshot
		},
	}}}
	p := newSpotProvider(f, "eu-west-2", SpotProviderOptions{})

	got, err := p.ListImages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := capacity.OwnedImage{
		Provider: providerName, Region: "eu-west-2", ImageID: "ami-1", Variant: "worker", State: "available",
		CreatedAt: time.Date(2026, 10, 21, 12, 30, 0, 0, time.UTC), Snapshots: []string{"snap-1"},
	}
	if len(got) != 1 || got[0].ImageID != want.ImageID || got[0].Variant != want.Variant || got[0].State != want.State ||
		!got[0].CreatedAt.Equal(want.CreatedAt) || !slices.Equal(got[0].Snapshots, want.Snapshots) || got[0].Region != want.Region {
		t.Fatalf("ListImages = %+v, want %+v", got, want)
	}
}

func TestListImageSnapshots_MapsVariant(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeEC2{snapshots: []ec2types.Snapshot{{SnapshotId: aws.String("snap-9"), StartTime: &start, Tags: amiTag("worker-nightly")}}}
	p := newSpotProvider(f, "us-east-2", SpotProviderOptions{})

	got, err := p.ListImageSnapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SnapshotID != "snap-9" || got[0].Variant != "worker-nightly" || !got[0].StartedAt.Equal(start) || got[0].Region != "us-east-2" {
		t.Fatalf("ListImageSnapshots = %+v", got)
	}
}

// Deregister then delete each snapshot; an image or snapshot already gone (another sweep got
// there first) is success, any other failure is reported.
func TestDeleteImage(t *testing.T) {
	t.Parallel()
	gone := &smithy.GenericAPIError{Code: "InvalidSnapshot.NotFound"}
	tests := []struct {
		name     string
		deregErr error
		snapErr  map[string]error
		wantErr  bool
		wantDel  []string
	}{
		{name: "deletes image and snapshots", wantDel: []string{"snap-a", "snap-b"}},
		{name: "image already gone", deregErr: &smithy.GenericAPIError{Code: "InvalidAMIID.Unavailable"}, wantDel: []string{"snap-a", "snap-b"}},
		{name: "snapshot already gone", snapErr: map[string]error{"snap-a": gone}, wantDel: []string{"snap-b"}},
		{name: "deregister refused", deregErr: &smithy.GenericAPIError{Code: "UnauthorizedOperation"}, wantErr: true},
		{name: "snapshot delete fails", snapErr: map[string]error{"snap-a": errors.New("throttled")}, wantErr: true, wantDel: []string{"snap-b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeEC2{deregErr: tt.deregErr, deleteSnapErr: tt.snapErr}
			p := newSpotProvider(f, "us-east-2", SpotProviderOptions{})
			err := p.DeleteImage(context.Background(), capacity.OwnedImage{ImageID: "ami-1", Snapshots: []string{"snap-a", "snap-b"}})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(f.deletedSnaps, tt.wantDel) {
				t.Fatalf("deleted %v, want %v", f.deletedSnaps, tt.wantDel)
			}
		})
	}
}
