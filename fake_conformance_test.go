package capacity_test

import (
	"testing"

	"github.com/getotium/capacity"
)

// TestFake_Conformance verifies the Fake satisfies the provider contract so the
// conformance suite itself is validated on every unit-test run.
func TestFake_Conformance(t *testing.T) {
	t.Parallel()
	const (
		model        = "test-model"
		instanceType = "g4dn.xlarge"
	)
	RunConformance(t, func(t *testing.T) capacity.Provider {
		t.Helper()
		f := capacity.NewFake()
		f.SetOffers(model, []capacity.Offer{{
			Provider:     "fake",
			InstanceType: instanceType,
			Region:       "us-east-1",
			Zone:         "us-east-1a",
			Kind:         capacity.KindSpot,
			PricePerHour: 0.30,
			Currency:     "USD",
		}})
		return f
	}, model, instanceType)
}
