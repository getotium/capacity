package aws

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/getotium/capacity"
)

// ListImages implements capacity.ImagePruner: every self-owned AMI in this region carrying the
// otium:ami tag (any variant, any state), with its backing snapshots. Paginated.
func (p *SpotProvider) ListImages(ctx context.Context) ([]capacity.OwnedImage, error) {
	input := &ec2.DescribeImagesInput{
		Owners:  []string{"self"},
		Filters: []ec2types.Filter{{Name: aws.String("tag-key"), Values: []string{tagAMI}}},
	}
	var out []capacity.OwnedImage
	for {
		page, err := p.client.DescribeImages(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("capacity/aws: list images %s: %w", p.region, err)
		}
		for _, img := range page.Images {
			oi := capacity.OwnedImage{
				Provider: providerName,
				Region:   p.region,
				ImageID:  aws.ToString(img.ImageId),
				Variant:  tagValue(img.Tags, tagAMI),
				State:    string(img.State),
			}
			if t, err := time.Parse(time.RFC3339, aws.ToString(img.CreationDate)); err == nil {
				oi.CreatedAt = t
			}
			for _, bd := range img.BlockDeviceMappings {
				if bd.Ebs != nil && bd.Ebs.SnapshotId != nil {
					oi.Snapshots = append(oi.Snapshots, aws.ToString(bd.Ebs.SnapshotId))
				}
			}
			out = append(out, oi)
		}
		if page.NextToken == nil {
			break
		}
		input.NextToken = page.NextToken
	}
	return out, nil
}

// ListImageSnapshots implements capacity.ImagePruner: every self-owned snapshot in this region
// carrying the otium:ami tag (Packer's snapshot_tags, in the build region and every copy).
// Paginated.
func (p *SpotProvider) ListImageSnapshots(ctx context.Context) ([]capacity.OwnedSnapshot, error) {
	input := &ec2.DescribeSnapshotsInput{
		OwnerIds: []string{"self"},
		Filters:  []ec2types.Filter{{Name: aws.String("tag-key"), Values: []string{tagAMI}}},
	}
	var out []capacity.OwnedSnapshot
	for {
		page, err := p.client.DescribeSnapshots(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("capacity/aws: list snapshots %s: %w", p.region, err)
		}
		for _, s := range page.Snapshots {
			os := capacity.OwnedSnapshot{
				Provider:   providerName,
				Region:     p.region,
				SnapshotID: aws.ToString(s.SnapshotId),
				Variant:    tagValue(s.Tags, tagAMI),
			}
			if s.StartTime != nil {
				os.StartedAt = *s.StartTime
			}
			out = append(out, os)
		}
		if page.NextToken == nil {
			break
		}
		input.NextToken = page.NextToken
	}
	return out, nil
}

// DeleteImage implements capacity.ImagePruner: deregister the AMI, then delete its snapshots
// (a snapshot cannot be deleted while a registered AMI uses it). Already gone counts as done.
func (p *SpotProvider) DeleteImage(ctx context.Context, img capacity.OwnedImage) error {
	_, err := p.client.DeregisterImage(ctx, &ec2.DeregisterImageInput{ImageId: aws.String(img.ImageID)})
	if err != nil && !isAWSCode(err, "InvalidAMIID.NotFound", "InvalidAMIID.Unavailable") {
		return fmt.Errorf("capacity/aws: deregister %s in %s: %w", img.ImageID, p.region, err)
	}
	var errs []error
	for _, id := range img.Snapshots {
		if err := p.deleteSnapshot(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// DeleteSnapshot implements capacity.ImagePruner. Already gone counts as done.
func (p *SpotProvider) DeleteSnapshot(ctx context.Context, snap capacity.OwnedSnapshot) error {
	return p.deleteSnapshot(ctx, snap.SnapshotID)
}

func (p *SpotProvider) deleteSnapshot(ctx context.Context, id string) error {
	_, err := p.client.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(id)})
	if err != nil && !isAWSCode(err, "InvalidSnapshot.NotFound") {
		return fmt.Errorf("capacity/aws: delete snapshot %s in %s: %w", id, p.region, err)
	}
	return nil
}

// tagValue returns the value of tag key, or "".
func tagValue(tags []ec2types.Tag, key string) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == key {
			return aws.ToString(t.Value)
		}
	}
	return ""
}

// isAWSCode reports whether err is an AWS API error with one of the given codes.
func isAWSCode(err error, codes ...string) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	for _, c := range codes {
		if ae.ErrorCode() == c {
			return true
		}
	}
	return false
}
