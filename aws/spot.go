// Package aws implements capacity.Provider for AWS EC2 Spot instances. It is the
// first real provider; the Fake in the parent package backs tests and local runs.
//
// Credentials come exclusively from the AWS default credential chain (SSO profile
// in local dev, instance role in production) — never from Otium configuration.
//
// Interruption notice: AWS delivers the 2-minute spot interruption notice via the
// instance metadata service (IMDS) at /latest/meta-data/spot/instance-action. The
// worker process on the instance polls IMDS and, on notice, deregisters from the
// workerapi before draining — so the control plane learns of the reclaim by push, not
// by polling. The provider holds no per-worker state; the durable registry (pkg/fleet)
// owns liveness. See docs/worker-registry.md.
package aws

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/getotium/capacity/awsx"
	"github.com/getotium/capacity"
)

const (
	providerName = "aws-spot"

	tagName     = "Name"
	tagModel    = "otium:model"
	tagWorker   = "otium:role"
	tagWorkerV  = "worker"
	tagWorkerID = "otium:worker-id" // the registry/handle id, for reconcile + ownership
	tagOwned    = "otium:owned"     // marks Otium-launched instances for discovery

	tagAMI       = "otium:ami" // Packer stamps this on the worker AMI for tag-based lookup
	tagAMIWorker = "worker"    // its value on the worker image

	// imageCacheTTL bounds how long a resolved-by-tag AMI id is reused before re-querying,
	// so a fresh bake is picked up within this window without a scheduler restart, while a
	// steady state costs at most one DescribeImages per TTL rather than one per launch.
	imageCacheTTL = 5 * time.Minute
	// networkCacheTTL bounds how long a discovered subnet/security-group set is reused. Subnets
	// and SGs are near-static (created by infra, rarely changed), so a longer TTL than the AMI's
	// is fine; a change is still picked up within the window without a scheduler restart.
	networkCacheTTL = 30 * time.Minute
)

// spotProviderAPI is the slice of the EC2 client the provider needs, making the
// real client substitutable with a mock in unit tests.
type spotProviderAPI interface {
	DescribeSpotPriceHistory(ctx context.Context, in *ec2.DescribeSpotPriceHistoryInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error)
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, optFns ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeImages(ctx context.Context, in *ec2.DescribeImagesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
	DescribeSubnets(ctx context.Context, in *ec2.DescribeSubnetsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	DescribeSecurityGroups(ctx context.Context, in *ec2.DescribeSecurityGroupsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
}

// SpotProvider implements capacity.Provider for AWS EC2 Spot instances.
// It is safe for concurrent use.
type SpotProvider struct {
	client spotProviderAPI
	region string
	log    *slog.Logger
	opts   SpotProviderOptions

	// Cache for the tag-resolved worker AMI, keyed by AMI variant tag value (used only when
	// no explicit image id is configured). Keyed because different models may launch on
	// different AMI lineages (otium:ami=worker vs a variant), each resolved independently.
	imgMu    sync.Mutex
	imgCache map[string]cachedImage
	nowFn    func() time.Time // injectable clock for tests; nil = time.Now

	// Cache for the tag-discovered launch network (subnet + security groups), keyed by
	// availability zone: a subnet is AZ-scoped, and its VPC's worker security groups come with
	// it. Used only when no explicit SubnetID/SecurityGroupIDs are configured, so a launch into
	// any region lands in the right VPC without per-region config.
	netMu    sync.Mutex
	netCache map[string]cachedNet
}

// cachedImage is one variant's resolved AMI id plus when it was resolved (for TTL expiry).
type cachedImage struct {
	id string
	at time.Time
}

// cachedNet is one AZ's discovered launch network (subnet + security groups) plus when it was
// resolved. A zero-value (empty subnet) is cached too, so an untagged account isn't re-queried
// every launch — it just falls back to AWS default-VPC selection until the TTL lapses.
type cachedNet struct {
	subnetID string
	sgIDs    []string
	at       time.Time
}

// SpotProviderOptions configures optional placement parameters for launched instances.
type SpotProviderOptions struct {
	// SubnetID constrains launches to a specific subnet. Empty means AWS selects
	// based on the default VPC for the account.
	SubnetID string
	// SecurityGroupIDs are the VPC security groups attached to the instance. Empty
	// means the account default security group.
	SecurityGroupIDs []string
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

var _ capacity.Provider = (*SpotProvider)(nil)

// NewSpotProvider builds a SpotProvider using live EC2 clients derived from cfg,
// pinned to region.
func NewSpotProvider(cfg aws.Config, region string, opts SpotProviderOptions) *SpotProvider {
	return newSpotProvider(awsx.EC2(cfg, region), region, opts)
}

// newSpotProvider is the testable constructor (accepts any spotProviderAPI).
func newSpotProvider(client spotProviderAPI, region string, opts SpotProviderOptions) *SpotProvider {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &SpotProvider{
		client: client,
		region: region,
		log:    opts.Logger,
		opts:   opts,
	}
}

// Name implements capacity.Provider.
func (p *SpotProvider) Name() string { return providerName }

// Quote implements capacity.Provider. It calls DescribeSpotPriceHistory to return
// the current spot price for each requested instance type in each AZ, filtered to
// Linux/UNIX workloads. One offer is returned per instance-type/AZ pair.
func (p *SpotProvider) Quote(ctx context.Context, req capacity.Requirement) ([]capacity.Offer, error) {
	types := toEC2InstanceTypes(req.InstanceTypes)
	if len(types) == 0 && len(req.InstanceTypes) > 0 {
		// The caller asked for specific types and none of them are EC2's — another cloud owns
		// this requirement. Quoting nothing is the correct answer; an empty InstanceTypes here
		// would instead ask EC2 to price its ENTIRE catalog.
		return nil, nil
	}
	input := &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:       types,
		ProductDescriptions: []string{"Linux/UNIX"},
		MaxResults:          aws.Int32(200),
	}
	out, err := p.client.DescribeSpotPriceHistory(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("capacity/aws: describe spot price history: %w", err)
	}

	// Deduplicate to newest price per (instance-type, AZ) pair.
	type key struct{ it, az string }
	newest := make(map[key]ec2types.SpotPrice)
	for _, sp := range out.SpotPriceHistory {
		k := key{string(sp.InstanceType), aws.ToString(sp.AvailabilityZone)}
		prev, ok := newest[k]
		if !ok || aws.ToTime(sp.Timestamp).After(aws.ToTime(prev.Timestamp)) {
			newest[k] = sp
		}
	}

	offers := make([]capacity.Offer, 0, len(newest))
	for _, sp := range newest {
		price, err := strconv.ParseFloat(aws.ToString(sp.SpotPrice), 64)
		if err != nil {
			continue
		}
		offers = append(offers, capacity.Offer{
			Provider:     providerName,
			InstanceType: string(sp.InstanceType),
			Region:       p.region,
			Zone:         aws.ToString(sp.AvailabilityZone),
			Kind:         capacity.KindSpot,
			PricePerHour: price,
			Currency:     "USD",
		})
	}
	return offers, nil
}

// Launch implements capacity.Provider. It calls RunInstances with a spot market
// request under the caller-supplied worker id (as ClientToken for idempotency and as an
// ownership tag) and returns a WorkerHandle carrying the EC2 instance ID.
func (p *SpotProvider) Launch(ctx context.Context, id string, offer capacity.Offer, spec capacity.WorkerSpec) (capacity.WorkerHandle, error) {
	// Resolve the launch image: an explicit id (OTIUM_WORKER_IMAGE) wins; otherwise find the
	// current worker AMI by tag (newest self-owned otium:ami=worker), so a fresh Packer bake
	// is used without editing config.
	image := spec.Image
	if image == "" {
		variant := spec.ImageVariant
		if variant == "" {
			variant = tagAMIWorker
		}
		resolved, err := p.resolveImage(ctx, variant)
		if err != nil {
			return capacity.WorkerHandle{}, fmt.Errorf("aws: resolve worker image: %w", err)
		}
		image = resolved
	}
	input := &ec2.RunInstancesInput{
		ImageId:      aws.String(image),
		InstanceType: ec2types.InstanceType(offer.InstanceType),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		// ClientToken makes RunInstances idempotent: a retry with the same worker id will
		// not launch a second instance.
		ClientToken: aws.String(id),
		InstanceMarketOptions: &ec2types.InstanceMarketOptionsRequest{
			MarketType: ec2types.MarketTypeSpot,
		},
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeInstance,
			Tags: []ec2types.Tag{
				{Key: aws.String(tagName), Value: aws.String("otium-worker-" + spec.Model)},
				{Key: aws.String(tagWorker), Value: aws.String(tagWorkerV)},
				{Key: aws.String(tagModel), Value: aws.String(spec.Model)},
				{Key: aws.String(tagWorkerID), Value: aws.String(id)},
				{Key: aws.String(tagOwned), Value: aws.String("true")},
			},
		}},
	}
	if spec.UserData != "" {
		input.UserData = aws.String(spec.UserData)
	}
	if spec.InstanceProfile != "" {
		input.IamInstanceProfile = &ec2types.IamInstanceProfileSpecification{
			Name: aws.String(spec.InstanceProfile),
		}
	}
	// Launch network: explicit opts win (single-region / pinned-VPC setups). Otherwise discover
	// a subnet in the offer's AZ and the security groups in its VPC, both tagged otium:role=worker
	// — so a launch into ANY region lands in the right VPC without per-region config. Discovery
	// degrades to AWS default-VPC selection if nothing is tagged or the describe is denied.
	subnetID, sgIDs := p.opts.SubnetID, p.opts.SecurityGroupIDs
	if subnetID == "" && len(sgIDs) == 0 {
		subnetID, sgIDs = p.resolveNetwork(ctx, offer.Zone)
	}
	if subnetID != "" {
		input.SubnetId = aws.String(subnetID)
	}
	if len(sgIDs) > 0 {
		input.SecurityGroupIds = sgIDs
	}
	if offer.Zone != "" {
		input.Placement = &ec2types.Placement{
			AvailabilityZone: aws.String(offer.Zone),
		}
	}

	out, err := p.client.RunInstances(ctx, input)
	if err != nil {
		return capacity.WorkerHandle{}, fmt.Errorf("capacity/aws: run instances model=%s: %w", spec.Model, err)
	}
	if len(out.Instances) == 0 {
		return capacity.WorkerHandle{}, fmt.Errorf("capacity/aws: run instances returned no instances")
	}

	instanceID := aws.ToString(out.Instances[0].InstanceId)
	p.log.InfoContext(ctx, "capacity/aws: launched spot instance",
		"worker", id, "instance", instanceID,
		"model", spec.Model, "instance_type", offer.InstanceType,
		"zone", offer.Zone, "price_per_hour", offer.PricePerHour)

	return capacity.WorkerHandle{
		ID:         id,
		InstanceID: instanceID,
		Provider:   providerName,
		Offer:      offer,
		Spec:       spec,
		LaunchedAt: time.Now(),
	}, nil
}

func (p *SpotProvider) now() time.Time {
	if p.nowFn != nil {
		return p.nowFn()
	}
	return time.Now()
}

// resolveImage returns the current worker AMI id for the given variant: the newest self-owned
// image tagged otium:ami=<variant> and in the available state. The result is cached per variant
// for imageCacheTTL so a fresh Packer bake is adopted within that window without a scheduler
// restart, while steady state costs at most one DescribeImages per variant per TTL. Used only
// when no explicit image id is configured.
func (p *SpotProvider) resolveImage(ctx context.Context, variant string) (string, error) {
	p.imgMu.Lock()
	defer p.imgMu.Unlock()
	if c, ok := p.imgCache[variant]; ok && p.now().Sub(c.at) < imageCacheTTL {
		return c.id, nil
	}
	out, err := p.client.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{"self"},
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + tagAMI), Values: []string{variant}},
			{Name: aws.String("state"), Values: []string{"available"}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("describe images: %w", err)
	}
	if len(out.Images) == 0 {
		return "", fmt.Errorf("no available AMI tagged %s=%s (build one: task packer:build)", tagAMI, variant)
	}
	// Newest by CreationDate (RFC3339 strings sort lexicographically in time order).
	newest := out.Images[0]
	for _, img := range out.Images[1:] {
		if aws.ToString(img.CreationDate) > aws.ToString(newest.CreationDate) {
			newest = img
		}
	}
	id := aws.ToString(newest.ImageId)
	if p.imgCache == nil {
		p.imgCache = make(map[string]cachedImage)
	}
	p.imgCache[variant] = cachedImage{id: id, at: p.now()}
	p.log.InfoContext(ctx, "capacity/aws: resolved worker AMI by tag",
		"ami", id, "variant", variant, "created", aws.ToString(newest.CreationDate))
	return id, nil
}

// resolveNetwork discovers the launch network for an AZ: a subnet tagged otium:role=worker in
// that zone, plus the security groups tagged the same in the subnet's VPC. The pair is resolved
// together (and cached per AZ for networkCacheTTL) so subnet and SGs are guaranteed same-VPC —
// RunInstances rejects a cross-VPC mix. Best-effort: any describe failure, or nothing tagged,
// returns empty so Launch falls back to AWS default-VPC selection (degrade, don't collapse). A
// zero result is cached too, so an untagged account is not re-queried on every launch.
func (p *SpotProvider) resolveNetwork(ctx context.Context, zone string) (string, []string) {
	if zone == "" {
		return "", nil
	}
	p.netMu.Lock()
	defer p.netMu.Unlock()
	if c, ok := p.netCache[zone]; ok && p.now().Sub(c.at) < networkCacheTTL {
		return c.subnetID, c.sgIDs
	}

	subnetID, sgIDs := "", []string(nil)
	sn, err := p.client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + tagWorker), Values: []string{tagWorkerV}},
			{Name: aws.String("availability-zone"), Values: []string{zone}},
		},
	})
	switch {
	case err != nil:
		p.log.WarnContext(ctx, "capacity/aws: subnet discovery failed; using default VPC",
			"zone", zone, "err", err)
	case len(sn.Subnets) == 0:
		p.log.WarnContext(ctx, "capacity/aws: no subnet tagged otium:role=worker; using default VPC",
			"zone", zone)
	default:
		subnetID = aws.ToString(sn.Subnets[0].SubnetId)
		vpcID := aws.ToString(sn.Subnets[0].VpcId)
		if vpcID != "" {
			sgIDs = p.discoverSecurityGroups(ctx, vpcID)
		}
		p.log.InfoContext(ctx, "capacity/aws: discovered launch network by tag",
			"zone", zone, "subnet", subnetID, "vpc", vpcID, "security_groups", len(sgIDs))
	}

	if p.netCache == nil {
		p.netCache = make(map[string]cachedNet)
	}
	p.netCache[zone] = cachedNet{subnetID: subnetID, sgIDs: sgIDs, at: p.now()}
	return subnetID, sgIDs
}

// discoverSecurityGroups returns the ids of the security groups tagged otium:role=worker in the
// given VPC. Called under netMu by resolveNetwork; a failure returns nil (default SG fallback).
func (p *SpotProvider) discoverSecurityGroups(ctx context.Context, vpcID string) []string {
	out, err := p.client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + tagWorker), Values: []string{tagWorkerV}},
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil {
		p.log.WarnContext(ctx, "capacity/aws: security-group discovery failed; using default SG",
			"vpc", vpcID, "err", err)
		return nil
	}
	ids := make([]string, 0, len(out.SecurityGroups))
	for _, sg := range out.SecurityGroups {
		ids = append(ids, aws.ToString(sg.GroupId))
	}
	return ids
}

// Terminate implements capacity.Provider. It calls TerminateInstances for the EC2
// instance the handle carries — no in-provider lookup, so a handle reconstructed from a
// durable registry row terminates correctly after a scheduler restart.
func (p *SpotProvider) Terminate(ctx context.Context, h capacity.WorkerHandle) error {
	if h.InstanceID == "" {
		return fmt.Errorf("capacity/aws: terminate worker %q: no instance id", h.ID)
	}
	_, err := p.client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{h.InstanceID},
	})
	if err != nil {
		return fmt.Errorf("capacity/aws: terminate instance %s: %w", h.InstanceID, err)
	}
	p.log.InfoContext(ctx, "capacity/aws: terminated spot instance",
		"worker", h.ID, "instance", h.InstanceID, "model", h.Spec.Model)
	return nil
}

// ListOwned implements capacity.Provider. It lists every pending/running instance
// tagged otium:owned=true — the provider's own inventory of what Otium launched,
// independent of the durable registry. Terminated/terminating instances are excluded
// (nothing to reap). Paginated so a large fleet is fully enumerated.
func (p *SpotProvider) ListOwned(ctx context.Context) ([]capacity.OwnedInstance, error) {
	input := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + tagOwned), Values: []string{"true"}},
			// Only states that still cost money and can be terminated.
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
		},
	}
	var owned []capacity.OwnedInstance
	for {
		out, err := p.client.DescribeInstances(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("capacity/aws: describe owned instances: %w", err)
		}
		for _, r := range out.Reservations {
			for _, inst := range r.Instances {
				oi := capacity.OwnedInstance{InstanceID: aws.ToString(inst.InstanceId)}
				if inst.LaunchTime != nil {
					oi.LaunchedAt = *inst.LaunchTime
				}
				for _, t := range inst.Tags {
					switch aws.ToString(t.Key) {
					case tagWorkerID:
						oi.WorkerID = aws.ToString(t.Value)
					case tagModel:
						oi.Model = aws.ToString(t.Value)
					}
				}
				owned = append(owned, oi)
			}
		}
		if out.NextToken == nil {
			break
		}
		input.NextToken = out.NextToken
	}
	return owned, nil
}

// ec2InstanceTypes is the set of instance-type names EC2 recognizes, from the SDK's own enum,
// so it tracks SDK upgrades instead of being a list we maintain.
var ec2InstanceTypes = func() map[string]bool {
	all := ec2types.InstanceType("").Values()
	m := make(map[string]bool, len(all))
	for _, t := range all {
		m[string(t)] = true
	}
	return m
}()

// toEC2InstanceTypes converts a string slice to ec2types.InstanceType, DROPPING names EC2 does
// not recognize.
//
// A model's candidate list is provider-neutral and may name machine types from several clouds
// (g4dn.xlarge alongside g2-standard-4), and MultiCloudProvider hands the whole list to every
// cloud. DescribeSpotPriceHistory rejects the entire call on one unknown instance type, and
// MultiCloudProvider deliberately skips a cloud whose Quote errors — so without this filter, a
// single GCP name in a model's list makes AWS silently vanish from that model's quotes and the
// work can only ever land on GCP. Dropping foreign names gives AWS the same shape the GCP and
// OCI providers already have: an instance type that isn't mine yields no offer, not an error.
func toEC2InstanceTypes(names []string) []ec2types.InstanceType {
	out := make([]ec2types.InstanceType, 0, len(names))
	for _, n := range names {
		if ec2InstanceTypes[n] {
			out = append(out, ec2types.InstanceType(n))
		}
	}
	return out
}
