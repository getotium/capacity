package aws

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/getotium/capacity"
)

// fakeEC2 stubs the spotProviderAPI for unit tests.
type fakeEC2 struct {
	spotHistory  []ec2types.SpotPrice
	spotErr      error
	launchedIDs  []string
	runErr       error
	terminateErr error
	instances    []ec2types.Reservation
	describeErr  error
	images       []ec2types.Image
	imagesErr    error
	seq          int

	// lastAMITag records the otium:ami tag value the last DescribeImages call filtered on,
	// so tests can assert the requested AMI variant is threaded into the query.
	lastAMITag string

	// Network discovery stubs (subnet + security groups tagged otium:role=worker).
	subnets   []ec2types.Subnet
	subnetErr error
	sgs       []ec2types.SecurityGroup
	sgErr     error
	// lastSubnetAZ records the availability-zone filter the last DescribeSubnets used.
	lastSubnetAZ string
	// last RunInstances placement, so tests can assert what discovery threaded into the launch.
	lastSubnetID string
	lastSGIDs    []string

	// spotCalls counts DescribeSpotPriceHistory calls and lastSpotTypes records the instance
	// types the last one asked for, so tests can assert foreign (non-EC2) names never reach AWS.
	spotCalls     int
	lastSpotTypes []string
}

func (f *fakeEC2) DescribeSubnets(_ context.Context, in *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	for _, filter := range in.Filters {
		if aws.ToString(filter.Name) == "availability-zone" && len(filter.Values) > 0 {
			f.lastSubnetAZ = filter.Values[0]
		}
	}
	if f.subnetErr != nil {
		return nil, f.subnetErr
	}
	return &ec2.DescribeSubnetsOutput{Subnets: f.subnets}, nil
}

func (f *fakeEC2) DescribeSecurityGroups(_ context.Context, _ *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	if f.sgErr != nil {
		return nil, f.sgErr
	}
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: f.sgs}, nil
}

func (f *fakeEC2) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	for _, filter := range in.Filters {
		if aws.ToString(filter.Name) == "tag:"+tagAMI && len(filter.Values) > 0 {
			f.lastAMITag = filter.Values[0]
		}
	}
	if f.imagesErr != nil {
		return nil, f.imagesErr
	}
	return &ec2.DescribeImagesOutput{Images: f.images}, nil
}

func (f *fakeEC2) DescribeSpotPriceHistory(_ context.Context, in *ec2.DescribeSpotPriceHistoryInput, _ ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error) {
	f.spotCalls++
	f.lastSpotTypes = nil
	for _, t := range in.InstanceTypes {
		f.lastSpotTypes = append(f.lastSpotTypes, string(t))
	}
	if f.spotErr != nil {
		return nil, f.spotErr
	}
	return &ec2.DescribeSpotPriceHistoryOutput{SpotPriceHistory: f.spotHistory}, nil
}

func (f *fakeEC2) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	if f.runErr != nil {
		return nil, f.runErr
	}
	f.lastSubnetID = aws.ToString(in.SubnetId)
	f.lastSGIDs = in.SecurityGroupIds
	f.seq++
	id := fmt.Sprintf("i-fake%04d", f.seq)
	f.launchedIDs = append(f.launchedIDs, id)
	return &ec2.RunInstancesOutput{
		Instances: []ec2types.Instance{{
			InstanceId:   aws.String(id),
			InstanceType: in.InstanceType,
			State: &ec2types.InstanceState{
				Name: ec2types.InstanceStateNamePending,
			},
		}},
	}, nil
}

func (f *fakeEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	if f.terminateErr != nil {
		return nil, f.terminateErr
	}
	changes := make([]ec2types.InstanceStateChange, len(in.InstanceIds))
	for i, id := range in.InstanceIds {
		changes[i] = ec2types.InstanceStateChange{InstanceId: aws.String(id)}
	}
	return &ec2.TerminateInstancesOutput{TerminatingInstances: changes}, nil
}

func (f *fakeEC2) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	return &ec2.DescribeInstancesOutput{Reservations: f.instances}, nil
}

func spotPrice(it, az, price string) ec2types.SpotPrice {
	return ec2types.SpotPrice{
		InstanceType:     ec2types.InstanceType(it),
		AvailabilityZone: aws.String(az),
		SpotPrice:        aws.String(price),
		Timestamp:        aws.Time(time.Now()),
	}
}

func newTestProvider(f *fakeEC2) *SpotProvider {
	return newSpotProvider(f, "us-east-1", SpotProviderOptions{})
}

func image(id, created string) ec2types.Image {
	return ec2types.Image{ImageId: aws.String(id), CreationDate: aws.String(created), State: ec2types.ImageStateAvailable}
}

func TestResolveImage(t *testing.T) {
	t.Parallel()

	t.Run("picks the newest tagged AMI", func(t *testing.T) {
		f := &fakeEC2{images: []ec2types.Image{
			image("ami-old", "2026-07-01T00:00:00.000Z"),
			image("ami-new", "2026-07-24T00:00:00.000Z"),
			image("ami-mid", "2026-07-10T00:00:00.000Z"),
		}}
		p := newTestProvider(f)
		got, err := p.resolveImage(context.Background(), "worker")
		if err != nil {
			t.Fatalf("resolveImage: %v", err)
		}
		if got != "ami-new" {
			t.Fatalf("resolved %s, want ami-new (newest)", got)
		}
	})

	t.Run("errors when no AMI is tagged", func(t *testing.T) {
		p := newTestProvider(&fakeEC2{images: nil})
		if _, err := p.resolveImage(context.Background(), "worker"); err == nil {
			t.Fatal("expected an error when no worker AMI exists")
		}
	})

	t.Run("caches within the TTL, re-queries after", func(t *testing.T) {
		f := &fakeEC2{images: []ec2types.Image{image("ami-1", "2026-07-24T00:00:00.000Z")}}
		p := newTestProvider(f)
		now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
		p.nowFn = func() time.Time { return now }

		if _, err := p.resolveImage(context.Background(), "worker"); err != nil {
			t.Fatalf("first resolve: %v", err)
		}
		// A newer AMI appears, but within the TTL the cache still serves the old id.
		f.images = []ec2types.Image{image("ami-2", "2026-07-24T06:00:00.000Z")}
		if got, _ := p.resolveImage(context.Background(), "worker"); got != "ami-1" {
			t.Fatalf("within TTL got %s, want cached ami-1", got)
		}
		// Past the TTL it re-queries and adopts the newer bake.
		now = now.Add(imageCacheTTL + time.Minute)
		if got, _ := p.resolveImage(context.Background(), "worker"); got != "ami-2" {
			t.Fatalf("after TTL got %s, want ami-2 (re-resolved)", got)
		}
	})

	t.Run("threads the variant into the tag filter", func(t *testing.T) {
		f := &fakeEC2{images: []ec2types.Image{image("ami-nightly", "2026-07-24T00:00:00.000Z")}}
		p := newTestProvider(f)
		if _, err := p.resolveImage(context.Background(), "worker-qwen35"); err != nil {
			t.Fatalf("resolveImage: %v", err)
		}
		if f.lastAMITag != "worker-qwen35" {
			t.Fatalf("filtered on otium:ami=%q, want worker-qwen35", f.lastAMITag)
		}
	})

	t.Run("caches each variant independently", func(t *testing.T) {
		// Both variants resolve from the same fake image set, but each is cached under its
		// own key — a second lookup of a variant must not return another variant's id.
		f := &fakeEC2{images: []ec2types.Image{image("ami-a", "2026-07-24T00:00:00.000Z")}}
		p := newTestProvider(f)
		if _, err := p.resolveImage(context.Background(), "worker"); err != nil {
			t.Fatalf("resolve worker: %v", err)
		}
		// A different image appears; the "worker" cache still serves ami-a, but a fresh
		// variant key must miss the cache and pick up the new image.
		f.images = []ec2types.Image{image("ami-b", "2026-07-24T06:00:00.000Z")}
		if got, _ := p.resolveImage(context.Background(), "worker"); got != "ami-a" {
			t.Fatalf("worker cache got %s, want ami-a", got)
		}
		if got, _ := p.resolveImage(context.Background(), "worker-qwen35"); got != "ami-b" {
			t.Fatalf("fresh variant got %s, want ami-b (own cache key, not worker's)", got)
		}
	})
}

func TestLaunch_ResolvesImageByTagWhenUnset(t *testing.T) {
	t.Parallel()
	f := &fakeEC2{images: []ec2types.Image{image("ami-tagged", "2026-07-24T00:00:00.000Z")}}
	p := newTestProvider(f)
	// No Image on the spec → the provider resolves it by tag.
	h, err := p.Launch(context.Background(), "w1", capacity.Offer{InstanceType: "g4dn.xlarge"}, capacity.WorkerSpec{Model: "llama"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if h.InstanceID == "" {
		t.Fatal("Launch returned no instance id")
	}
}

func TestSpotProvider_Name(t *testing.T) {
	t.Parallel()
	p := newTestProvider(&fakeEC2{})
	if p.Name() != providerName {
		t.Errorf("Name() = %q, want %q", p.Name(), providerName)
	}
}

func TestSpotProvider_Quote_ReturnsOffersForMatchingTypes(t *testing.T) {
	t.Parallel()
	f := &fakeEC2{
		spotHistory: []ec2types.SpotPrice{
			spotPrice("g4dn.xlarge", "us-east-1a", "0.25"),
			spotPrice("g4dn.xlarge", "us-east-1b", "0.28"),
			spotPrice("g6.xlarge", "us-east-1a", "0.30"),
		},
	}
	p := newTestProvider(f)

	offers, err := p.Quote(context.Background(), capacity.Requirement{
		Model:         "llama",
		InstanceTypes: []string{"g4dn.xlarge", "g6.xlarge"},
		Region:        "us-east-1",
	})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if len(offers) != 3 {
		t.Errorf("got %d offers, want 3", len(offers))
	}
	for _, o := range offers {
		if o.Provider != providerName {
			t.Errorf("offer.Provider = %q, want %q", o.Provider, providerName)
		}
		if o.Kind != capacity.KindSpot {
			t.Errorf("offer.Kind = %q, want spot", o.Kind)
		}
		if o.PricePerHour <= 0 {
			t.Errorf("offer.PricePerHour = %v, want > 0", o.PricePerHour)
		}
	}
}

func TestSpotProvider_Quote_DeduplicatesToNewest(t *testing.T) {
	t.Parallel()
	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now().Add(-5 * time.Minute)
	f := &fakeEC2{
		spotHistory: []ec2types.SpotPrice{
			{InstanceType: "g4dn.xlarge", AvailabilityZone: aws.String("us-east-1a"), SpotPrice: aws.String("0.50"), Timestamp: aws.Time(old)},
			{InstanceType: "g4dn.xlarge", AvailabilityZone: aws.String("us-east-1a"), SpotPrice: aws.String("0.25"), Timestamp: aws.Time(recent)},
		},
	}
	p := newTestProvider(f)
	offers, err := p.Quote(context.Background(), capacity.Requirement{
		Model: "llama", InstanceTypes: []string{"g4dn.xlarge"}, Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if len(offers) != 1 {
		t.Fatalf("got %d offers, want 1", len(offers))
	}
	if offers[0].PricePerHour != 0.25 {
		t.Errorf("expected newest price 0.25, got %v", offers[0].PricePerHour)
	}
}

// A model's candidate list is provider-neutral and MultiCloudProvider hands the whole list to
// every cloud. DescribeSpotPriceHistory rejects the entire call on one unrecognized instance
// type, and MultiCloudProvider skips a cloud whose Quote errors — so an unfiltered GCP name in
// the list would make AWS silently disappear from that model's quotes.
func TestSpotProvider_Quote_IgnoresForeignInstanceTypes(t *testing.T) {
	t.Parallel()
	f := &fakeEC2{spotHistory: []ec2types.SpotPrice{spotPrice("g4dn.xlarge", "us-east-1a", "0.25")}}
	p := newTestProvider(f)

	offers, err := p.Quote(context.Background(), capacity.Requirement{
		Model:         "llama",
		InstanceTypes: []string{"g4dn.xlarge", "g2-standard-4", "n1-standard-4"},
		Region:        "us-east-1",
	})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if len(offers) != 1 {
		t.Fatalf("got %d offers, want 1", len(offers))
	}
	if want := []string{"g4dn.xlarge"}; !slices.Equal(f.lastSpotTypes, want) {
		t.Errorf("EC2 asked for %v, want only %v — GCP names must never reach the API", f.lastSpotTypes, want)
	}
}

// The other half: when NONE of the requested types are EC2's, quote nothing and never call the
// API. Passing an empty InstanceTypes through would ask EC2 to price its entire catalog.
func TestSpotProvider_Quote_AllForeignTypesSkipsAPI(t *testing.T) {
	t.Parallel()
	f := &fakeEC2{spotHistory: []ec2types.SpotPrice{spotPrice("g4dn.xlarge", "us-east-1a", "0.25")}}
	p := newTestProvider(f)

	offers, err := p.Quote(context.Background(), capacity.Requirement{
		Model:         "llama",
		InstanceTypes: []string{"g2-standard-4", "a2-ultragpu-1g"},
		Region:        "us-east-1",
	})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if len(offers) != 0 {
		t.Errorf("got %d offers, want 0 — none of these types are EC2's", len(offers))
	}
	if f.spotCalls != 0 {
		t.Errorf("DescribeSpotPriceHistory called %d times; a request with no EC2 types must not hit the API", f.spotCalls)
	}
}

func TestSpotProvider_Quote_Error(t *testing.T) {
	t.Parallel()
	f := &fakeEC2{spotErr: fmt.Errorf("aws: throttled")}
	p := newTestProvider(f)
	_, err := p.Quote(context.Background(), capacity.Requirement{Model: "llama"})
	if err == nil {
		t.Fatal("expected error from Quote when API fails, got nil")
	}
}

func TestSpotProvider_Launch_Success(t *testing.T) {
	t.Parallel()
	f := &fakeEC2{}
	p := newTestProvider(f)

	offer := capacity.Offer{
		Provider: providerName, InstanceType: "g4dn.xlarge",
		Region: "us-east-1", Zone: "us-east-1a",
		Kind: capacity.KindSpot, PricePerHour: 0.25,
	}
	spec := capacity.WorkerSpec{
		Model:           "llama",
		Image:           "ami-0abc123",
		InstanceProfile: "otium-worker-profile",
	}

	h, err := p.Launch(context.Background(), "w-launch", offer, spec)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if h.ID != "w-launch" {
		t.Errorf("handle ID = %q, want the supplied w-launch", h.ID)
	}
	if h.InstanceID == "" {
		t.Error("handle InstanceID is empty")
	}
	if h.Provider != providerName {
		t.Errorf("handle.Provider = %q, want %q", h.Provider, providerName)
	}
	if h.Spec.Model != spec.Model {
		t.Errorf("handle.Spec.Model = %q, want %q", h.Spec.Model, spec.Model)
	}
	if len(f.launchedIDs) != 1 {
		t.Errorf("expected 1 RunInstances call, got %d", len(f.launchedIDs))
	}
}

func TestSpotProvider_Launch_DiscoversNetwork(t *testing.T) {
	t.Parallel()
	f := &fakeEC2{
		subnets: []ec2types.Subnet{{SubnetId: aws.String("subnet-abc"), VpcId: aws.String("vpc-1")}},
		sgs: []ec2types.SecurityGroup{
			{GroupId: aws.String("sg-1")}, {GroupId: aws.String("sg-2")},
		},
	}
	p := newTestProvider(f) // no explicit SubnetID/SecurityGroupIDs → discovery kicks in

	offer := capacity.Offer{
		Provider: providerName, InstanceType: "g4dn.xlarge",
		Region: "us-east-1", Zone: "us-east-1b", Kind: capacity.KindSpot, PricePerHour: 0.25,
	}
	if _, err := p.Launch(context.Background(), "w1", offer, capacity.WorkerSpec{Model: "m", Image: "ami-x"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if f.lastSubnetAZ != "us-east-1b" {
		t.Errorf("subnet discovery filtered AZ %q, want us-east-1b", f.lastSubnetAZ)
	}
	if f.lastSubnetID != "subnet-abc" {
		t.Errorf("RunInstances SubnetId = %q, want the discovered subnet-abc", f.lastSubnetID)
	}
	if len(f.lastSGIDs) != 2 || f.lastSGIDs[0] != "sg-1" || f.lastSGIDs[1] != "sg-2" {
		t.Errorf("RunInstances SecurityGroupIds = %v, want [sg-1 sg-2]", f.lastSGIDs)
	}
}

func TestSpotProvider_Launch_NetworkDiscoveryDegrades(t *testing.T) {
	t.Parallel()
	// Describe denied (e.g. missing IAM) — the launch must still succeed, unpinned, so AWS
	// selects the default VPC. Degrade, don't collapse.
	f := &fakeEC2{subnetErr: fmt.Errorf("AccessDenied: not authorized")}
	p := newTestProvider(f)

	offer := capacity.Offer{Provider: providerName, InstanceType: "g4dn.xlarge", Region: "us-east-1", Zone: "us-east-1a", Kind: capacity.KindSpot}
	if _, err := p.Launch(context.Background(), "w1", offer, capacity.WorkerSpec{Model: "m", Image: "ami-x"}); err != nil {
		t.Fatalf("Launch should degrade, not fail: %v", err)
	}
	if f.lastSubnetID != "" || len(f.lastSGIDs) != 0 {
		t.Errorf("expected no subnet/SG pinned on discovery failure, got subnet=%q sgs=%v", f.lastSubnetID, f.lastSGIDs)
	}
}

func TestSpotProvider_Launch_ExplicitNetworkSkipsDiscovery(t *testing.T) {
	t.Parallel()
	f := &fakeEC2{subnets: []ec2types.Subnet{{SubnetId: aws.String("subnet-discovered"), VpcId: aws.String("vpc-1")}}}
	p := newSpotProvider(f, "us-east-1", SpotProviderOptions{
		SubnetID: "subnet-explicit", SecurityGroupIDs: []string{"sg-explicit"},
	})

	offer := capacity.Offer{Provider: providerName, InstanceType: "g4dn.xlarge", Region: "us-east-1", Zone: "us-east-1a", Kind: capacity.KindSpot}
	if _, err := p.Launch(context.Background(), "w1", offer, capacity.WorkerSpec{Model: "m", Image: "ami-x"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if f.lastSubnetAZ != "" {
		t.Error("explicit opts should skip discovery, but DescribeSubnets was called")
	}
	if f.lastSubnetID != "subnet-explicit" || len(f.lastSGIDs) != 1 || f.lastSGIDs[0] != "sg-explicit" {
		t.Errorf("explicit network not used: subnet=%q sgs=%v", f.lastSubnetID, f.lastSGIDs)
	}
}

func TestSpotProvider_Launch_APIError(t *testing.T) {
	t.Parallel()
	f := &fakeEC2{runErr: fmt.Errorf("aws: insufficient capacity")}
	p := newTestProvider(f)
	_, err := p.Launch(context.Background(), "w-err", capacity.Offer{InstanceType: "g4dn.xlarge"}, capacity.WorkerSpec{Model: "llama"})
	if err == nil {
		t.Fatal("expected error when RunInstances fails, got nil")
	}
}

func TestSpotProvider_Terminate_Success(t *testing.T) {
	t.Parallel()
	f := &fakeEC2{}
	p := newTestProvider(f)

	// Launch first so Terminate knows the instance ID.
	offer := capacity.Offer{InstanceType: "g4dn.xlarge", Zone: "us-east-1a"}
	h, err := p.Launch(context.Background(), "w-term", offer, capacity.WorkerSpec{Model: "llama", Image: "ami-0"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := p.Terminate(context.Background(), h); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
}

func TestSpotProvider_Terminate_UnknownHandle(t *testing.T) {
	t.Parallel()
	p := newTestProvider(&fakeEC2{})
	err := p.Terminate(context.Background(), capacity.WorkerHandle{ID: "not-tracked"})
	if err == nil {
		t.Fatal("expected error for unknown handle, got nil")
	}
}
