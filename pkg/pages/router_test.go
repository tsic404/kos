package pages

import (
	"context"
	"testing"
	"time"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/write"
)

// routerReadings answers every item with its own product identifier.
type routerReadings struct {
	product string
}

// Read implements Readings.
func (r routerReadings) Read(_ context.Context, _ identity.Session, item extension.PageItem) (Reading, *controlplane.Error) {
	return Reading{Value: r.product, ObservedAt: time.Now()}, nil
}

// TestRouterDispatchesByProductReference checks that one panel can render the
// pages of several products: each item goes to the product its reference names
// and an unrouted item is refused, never answered with another product's value
// (C-22, C-53).
func TestRouterDispatchesByProductReference(t *testing.T) {
	router := Router{
		"kos-taskmanager": routerReadings{product: "kos-taskmanager"},
		"kos-storage":     routerReadings{product: "kos-storage"},
	}
	session := identity.Session{Username: "alice", Role: write.RoleOwner}
	cases := []struct {
		name      string
		reference string
		want      string
		wantCode  string
	}{
		{name: "first product", reference: "kos-taskmanager/nodes-ready", want: "kos-taskmanager"},
		{name: "second product", reference: "kos-storage/volumes", want: "kos-storage"},
		{name: "unknown product", reference: "kos-absent/page", wantCode: controlplane.CodeCapabilityMissing},
		{name: "no prefix", reference: "nodes-ready", wantCode: controlplane.CodeCapabilityMissing},
		{name: "empty key", reference: "kos-storage/", wantCode: controlplane.CodeCapabilityMissing},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			reading, failure := router.Read(context.Background(), session, extension.PageItem{
				ID: "item", Reference: testCase.reference,
			})
			if testCase.wantCode != "" {
				if failure == nil || failure.Code != testCase.wantCode {
					t.Fatalf("failure = %+v, want code %q", failure, testCase.wantCode)
				}
				return
			}
			if failure != nil {
				t.Fatalf("read: %+v", failure)
			}
			if reading.Value != testCase.want {
				t.Fatalf("value = %v, want the %s reader", reading.Value, testCase.want)
			}
		})
	}
}

// TestReferencePrefix checks the reference grammar the router keys on.
func TestReferencePrefix(t *testing.T) {
	cases := []struct {
		reference string
		product   string
		key       string
		ok        bool
	}{
		{reference: "kos-storage/volumes", product: "kos-storage", key: "volumes", ok: true},
		{reference: "kos-storage/volume/usage", product: "kos-storage", key: "volume/usage", ok: true},
		{reference: "/page", ok: false},
		{reference: "page/", ok: false},
		{reference: "page", ok: false},
	}
	for _, testCase := range cases {
		product, key, ok := ReferencePrefix(testCase.reference)
		if product != testCase.product || key != testCase.key || ok != testCase.ok {
			t.Errorf("ReferencePrefix(%q) = (%q, %q, %v), want (%q, %q, %v)",
				testCase.reference, product, key, ok, testCase.product, testCase.key, testCase.ok)
		}
	}
}
