package kostaskmanager

import (
	"context"
	"testing"

	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/write"
)

// usageSource answers one fixed sample set.
type usageSource []ApplicationUsage

// Usage implements UsageSource.
func (u usageSource) Usage(context.Context, identity.Session) ([]ApplicationUsage, error) {
	return u, nil
}

// TestResourceUsageRendersTheSampledValues checks that a connected sampler is
// what the metric item shows: the sample's values and the read time land in the
// item (C-49, C-53).
func TestResourceUsageRendersTheSampledValues(t *testing.T) {
	readings := &Readings{Usage: usageSource{{
		Namespace: "default", Name: "shop", CPU: "250m", Memory: "128Mi",
	}}}
	reading, failure := readings.Read(context.Background(),
		identity.Session{Username: "alice", Role: write.RoleOwner},
		extension.PageItem{ID: ItemResourceUsage, Source: extension.PageSourceMetric})
	if failure != nil {
		t.Fatalf("read usage: %+v", failure)
	}
	value, ok := reading.Value.(map[string]any)
	if !ok || value["available"] != true {
		t.Fatalf("value = %v, want a sampled value", reading.Value)
	}
	samples, ok := value["applications"].([]ApplicationUsage)
	if !ok || len(samples) != 1 || samples[0].CPU != "250m" || samples[0].Name != "shop" {
		t.Fatalf("applications = %v, want the sampled CPU of shop", value["applications"])
	}
	if reading.ObservedAt.IsZero() {
		t.Fatalf("observation time = %v, want the read time", reading.ObservedAt)
	}
}

// TestResourceUsageStatesAnAbsentSampler checks the item's answer when the
// distribution connects no sampler: it names the gap instead of presenting a
// number (C-49).
func TestResourceUsageStatesAnAbsentSampler(t *testing.T) {
	readings := &Readings{}
	reading, failure := readings.Read(context.Background(),
		identity.Session{Username: "alice", Role: write.RoleOwner},
		extension.PageItem{ID: ItemResourceUsage, Source: extension.PageSourceMetric})
	if failure != nil {
		t.Fatalf("read usage: %+v", failure)
	}
	value, ok := reading.Value.(map[string]any)
	if !ok || value["available"] != false {
		t.Fatalf("value = %v, want the absent-sampler answer", reading.Value)
	}
	if reason, _ := value["reason"].(string); reason == "" {
		t.Fatalf("value = %v, want a stated reason", value)
	}
}
