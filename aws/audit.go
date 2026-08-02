package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/getotium/capacity"
)

// AuditInstances implements capacity.InstanceAuditor: every pending/running instance in this
// provider's region, OWNED OR NOT. Unlike ListOwned it applies no ownership filter — the whole
// point is to see what the reaper can't, e.g. a leaked Packer builder (untagged, so ListOwned
// never returns it) still billing days later. Paginated.
func (p *SpotProvider) AuditInstances(ctx context.Context) ([]capacity.AuditedInstance, error) {
	input := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			// Only states that still cost money; no owner-tag filter — this is the whole account.
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running"}},
		},
	}
	var out []capacity.AuditedInstance
	for {
		page, err := p.client.DescribeInstances(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("capacity/aws: audit instances %s: %w", p.region, err)
		}
		for _, r := range page.Reservations {
			for _, inst := range r.Instances {
				ai := capacity.AuditedInstance{
					Provider:     providerName,
					Region:       p.region,
					InstanceID:   aws.ToString(inst.InstanceId),
					InstanceType: string(inst.InstanceType),
				}
				if inst.LaunchTime != nil {
					ai.LaunchedAt = *inst.LaunchTime
				}
				for _, t := range inst.Tags {
					switch aws.ToString(t.Key) {
					case tagName:
						ai.Name = aws.ToString(t.Value)
					case tagOwned:
						ai.Owned = aws.ToString(t.Value) == "true"
					}
				}
				out = append(out, ai)
			}
		}
		if page.NextToken == nil {
			break
		}
		input.NextToken = page.NextToken
	}
	return out, nil
}
