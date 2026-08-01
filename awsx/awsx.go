// Package awsx holds the tiny shared AWS plumbing for Otium's provider sources: how to
// resolve credentials and build per-region EC2 clients. It exists so pkg/pricing/aws and
// pkg/inventory/aws don't each reinvent client construction.
//
// Credentials are supplied explicitly via WithStaticCredentials — the per-identity
// OTIUM_AWS_*_ACCESS_KEY / _SECRET_KEY a caller reads from config (a read-only pricing user
// for otium-ops, a write provisioning user for otium-scheduler). When none are given it
// falls back to the AWS default credential chain (SSO profile in local dev, instance role in
// production), so an unset pair is a no-op. See docs/pricing-indexer.md.
package awsx

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
)

// PricingRegion is the region the AWS Price List Query API is pinned to. The service is
// global but reachable only from a few regions; us-east-1 is the canonical one. On-demand
// prices are region-level data carried in the response regardless of this client region.
const PricingRegion = "us-east-1"

// Option configures LoadConfig.
type Option func(*loadOptions)

type loadOptions struct {
	accessKey string
	secretKey string
}

// WithStaticCredentials supplies an explicit AWS access key + secret. When BOTH are
// non-empty LoadConfig uses them (a static credentials provider) instead of the default
// credential chain; when either is empty the option is a no-op and the chain resolves
// credentials. IAM-user keys carry no session token, so none is passed.
func WithStaticCredentials(accessKey, secretKey string) Option {
	return func(o *loadOptions) {
		o.accessKey = accessKey
		o.secretKey = secretKey
	}
}

// LoadConfig resolves AWS configuration. region sets the default region for clients derived
// from the returned config; pass "" to defer to the environment/profile default. Credentials
// come from WithStaticCredentials when supplied, otherwise the AWS default credential chain.
func LoadConfig(ctx context.Context, region string, opts ...Option) (aws.Config, error) {
	var lo loadOptions
	for _, fn := range opts {
		fn(&lo)
	}

	var cfgOpts []func(*awsconfig.LoadOptions) error
	if region != "" {
		cfgOpts = append(cfgOpts, awsconfig.WithRegion(region))
	}
	if lo.accessKey != "" && lo.secretKey != "" {
		cfgOpts = append(cfgOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(lo.accessKey, lo.secretKey, "")))
	}
	return awsconfig.LoadDefaultConfig(ctx, cfgOpts...)
}

// EC2 builds an EC2 client from cfg pinned to region (overriding cfg's default region).
// Spot prices are region-scoped, so the per-region sources build one client each.
func EC2(cfg aws.Config, region string) *ec2.Client {
	return ec2.NewFromConfig(cfg, func(o *ec2.Options) {
		if region != "" {
			o.Region = region
		}
	})
}

// Pricing builds an AWS Price List Query API client from cfg, pinned to PricingRegion.
// The on-demand price source asks one global endpoint for every region's prices, so the
// client region is fixed regardless of which worker regions are enabled.
func Pricing(cfg aws.Config) *pricing.Client {
	return pricing.NewFromConfig(cfg, func(o *pricing.Options) {
		o.Region = PricingRegion
		// The Price List API has a low, unpublished request rate and throttles a full region×type
		// sweep. Adaptive retry adds an SDK-side rate limiter that backs off on ThrottlingException
		// instead of failing after the default 3 attempts, and rides out a transient spike; the
		// on-demand source also paces its own calls. More attempts give the backoff room to work.
		o.Retryer = retry.NewAdaptiveMode(func(ao *retry.AdaptiveModeOptions) {
			ao.StandardOptions = append(ao.StandardOptions, func(so *retry.StandardOptions) {
				so.MaxAttempts = 8
			})
		})
	})
}
