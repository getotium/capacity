package aws

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/getotium/capacity"
)

func TestAuditInstances_MapsOwnedAndUnowned(t *testing.T) {
	t.Parallel()
	launch := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	f := &fakeEC2{instances: []ec2types.Reservation{{Instances: []ec2types.Instance{
		{
			InstanceId:   aws.String("i-owned"),
			InstanceType: ec2types.InstanceType("g4dn.xlarge"),
			LaunchTime:   &launch,
			Tags: []ec2types.Tag{
				{Key: aws.String("otium:owned"), Value: aws.String("true")},
				{Key: aws.String("Name"), Value: aws.String("otium-worker-x")},
			},
		},
		{
			// No otium:owned tag → the leak case (e.g. a stranded Packer builder ListOwned misses).
			InstanceId:   aws.String("i-leak"),
			InstanceType: ec2types.InstanceType("g4dn.xlarge"),
			LaunchTime:   &launch,
		},
	}}}}
	p := newSpotProvider(f, "us-east-2", SpotProviderOptions{})

	got, err := p.AuditInstances(context.Background())
	if err != nil {
		t.Fatalf("AuditInstances: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d instances, want 2 (owned + unowned — audit ignores ownership)", len(got))
	}
	byID := map[string]capacity.AuditedInstance{}
	for _, a := range got {
		byID[a.InstanceID] = a
	}
	o := byID["i-owned"]
	if !o.Owned || o.Region != "us-east-2" || o.Provider != providerName || o.Name != "otium-worker-x" || !o.LaunchedAt.Equal(launch) {
		t.Fatalf("i-owned mapped wrong: %+v", o)
	}
	if l := byID["i-leak"]; l.Owned {
		t.Fatalf("i-leak must be Owned=false (no otium:owned tag): %+v", l)
	}
}
