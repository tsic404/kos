package kosstorage

import (
	"context"
	"testing"

	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/write"
)

// usageSource answers one fixed sample set.
type usageSource []VolumeUsage

// Usage implements UsageSource.
func (u usageSource) Usage(context.Context) ([]VolumeUsage, error) {
	return u, nil
}

// TestUsageRendersTheSampledFill checks that a connected sampler is what the
// usage item shows (C-49, C-53).
func TestUsageRendersTheSampledFill(t *testing.T) {
	readings := &Readings{Usage: usageSource{{
		Namespace: "default", Name: "data", Used: "400Mi", Capacity: "1Gi",
	}}}
	reading, failure := readings.Read(context.Background(),
		identity.Session{Username: "alice", Role: write.RoleOwner},
		extension.PageItem{ID: ItemUsage, Source: extension.PageSourceMetric})
	if failure != nil {
		t.Fatalf("read usage: %+v", failure)
	}
	value, ok := reading.Value.(map[string]any)
	if !ok || value["available"] != true {
		t.Fatalf("value = %v, want a sampled value", reading.Value)
	}
	samples, ok := value["volumes"].([]VolumeUsage)
	if !ok || len(samples) != 1 || samples[0].Used != "400Mi" || samples[0].Name != "data" {
		t.Fatalf("volumes = %v, want the sampled fill of data", value["volumes"])
	}
	if reading.ObservedAt.IsZero() {
		t.Fatalf("observation time = %v, want the read time", reading.ObservedAt)
	}
}

// TestUsageStatesAnAbsentSampler checks the item's answer when the distribution
// connects no volume sampler (C-49).
func TestUsageStatesAnAbsentSampler(t *testing.T) {
	readings := &Readings{}
	reading, failure := readings.Read(context.Background(),
		identity.Session{Username: "alice", Role: write.RoleOwner},
		extension.PageItem{ID: ItemUsage, Source: extension.PageSourceMetric})
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
