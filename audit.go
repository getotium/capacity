package capacity

import (
	"context"
	"time"
)

// AuditedInstance is one running compute instance in a provider's account/project — EVERY
// instance, not just the ones Otium launched. It is what an account-wide cost-leak watchdog
// scans, so it can flag anything running longer than it should (a leaked image-build VM, a box
// someone forgot) that the ownership-scoped reaper never sees.
type AuditedInstance struct {
	Provider     string    // the cloud ("aws", "gcp", …), for cross-cloud labels/routing
	Region       string    // the region/zone it runs in
	InstanceID   string    // the provider machine id
	InstanceType string    // e.g. "g4dn.xlarge"
	Name         string    // best-effort human label (Name tag / label); "" if none
	Owned        bool      // carries Otium's ownership tag/label — i.e. the reaper's domain
	LaunchedAt   time.Time // when it started; zero if the provider didn't report it
}

// InstanceAuditor lists ALL running instances in a provider's account (across the regions it
// covers). It is a SEPARATE, OPTIONAL capability from Provider — provider-agnostic on purpose, so
// the same leak watchdog works for AWS today and GCP/Azure/OCI later with no change to the alert
// path. It deliberately ignores ownership, so it catches leaks that ListOwned (and thus the
// reaper) cannot. A provider that doesn't implement it is simply skipped by the auditor.
type InstanceAuditor interface {
	Name() string
	AuditInstances(ctx context.Context) ([]AuditedInstance, error)
}
