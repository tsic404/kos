package markers

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// contractMarkers is C-02's marker table spelled out wire key by wire key with
// the carrier and class the contract gives it. The expectation is the contract
// rather than Keys(), so dropping a key cannot shrink it too.
var contractMarkers = map[string]struct {
	carrier Carrier
	class   Class
}{
	"managed-by":      {carrier: CarrierLabel, class: ClassAttribution},
	"owner-uid":       {carrier: CarrierLabel, class: ClassAttribution},
	"adopted-by":      {carrier: CarrierLabel, class: ClassAttribution},
	"adoption-reason": {carrier: CarrierLabel, class: ClassAttribution},
	"adopted-at":      {carrier: CarrierAnnotation, class: ClassAttribution},
	"declined":        {carrier: CarrierLabel, class: ClassDecline},
	"released":        {carrier: CarrierLabel, class: ClassRelease},
	"invalid":         {carrier: CarrierLabel, class: ClassValidity},
	"stale":           {carrier: CarrierLabel, class: ClassValidity},
}

func TestKeysMatchContract(t *testing.T) {
	want := make([]string, 0, len(contractMarkers))
	for key := range contractMarkers {
		want = append(want, key)
	}
	sort.Strings(want)
	if got := Keys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Keys() = %v, want %v (C-02)", got, want)
	}
}

func TestCarrierAndClassMatchContract(t *testing.T) {
	for key, want := range contractMarkers {
		t.Run(key, func(t *testing.T) {
			carrier, ok := CarrierOf(key)
			if !ok {
				t.Fatalf("CarrierOf(%q) reported no carrier", key)
			}
			if carrier != want.carrier {
				t.Errorf("CarrierOf(%q) = %q, want %q", key, carrier, want.carrier)
			}
			class, ok := ClassOf(key)
			if !ok {
				t.Fatalf("ClassOf(%q) reported no class", key)
			}
			if class != want.class {
				t.Errorf("ClassOf(%q) = %q, want %q", key, class, want.class)
			}
		})
	}
}

func TestValidateRejectsMisplacedKeys(t *testing.T) {
	tests := map[string]struct {
		labels      map[string]string
		annotations map[string]string
		wantErr     string
	}{
		"marker on annotations": {
			annotations: map[string]string{ManagedBy: "kos-shell"},
			wantErr:     `"managed-by" belongs on label, not annotation`,
		},
		"adopted-at on labels": {
			labels:  map[string]string{AdoptedAt: "2026-10-25T00:00:00Z"},
			wantErr: `"adopted-at" belongs on annotation, not label`,
		},
		"conflicting attribution": {
			labels: map[string]string{
				ManagedBy:      "kos-shell",
				AdoptedBy:      "kos-shell",
				AdoptionReason: "operator",
			},
			wantErr: `"managed-by" and "adopted-by" are both present`,
		},
		"conflict with reason alone names the present key": {
			labels: map[string]string{
				ManagedBy:      "kos-shell",
				AdoptionReason: "operator",
			},
			wantErr: `"managed-by" and "adoption-reason" are both present`,
		},
		"half adoption pair": {
			labels:  map[string]string{AdoptedBy: "kos-shell"},
			wantErr: "incomplete adoption",
		},
		"adopted without adopted-at": {
			labels: map[string]string{
				AdoptedBy:      "kos-shell",
				AdoptionReason: "operator",
			},
			wantErr: `lacks the "adopted-at" annotation`,
		},
		"unknown annotation": {
			annotations: map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"},
		},
		"managed with owner": {
			labels: map[string]string{ManagedBy: "kos-shell", OwnerUID: "abc"},
		},
		"unmanaged": {},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := Validate(tc.labels, tc.annotations)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestAttributionOf(t *testing.T) {
	tests := map[string]struct {
		labels  map[string]string
		want    Attribution
		wantErr bool
	}{
		"managed":               {labels: map[string]string{ManagedBy: "kos-shell"}, want: AttributionManaged},
		"unmanaged":             {want: AttributionUnmanaged},
		"adopted":               {labels: map[string]string{AdoptedBy: "kos-shell", AdoptionReason: "operator"}, want: AttributionAdopted},
		"declined is unmanaged": {labels: map[string]string{Declined: "true"}, want: AttributionUnmanaged},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := AttributionOf(tc.labels)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("AttributionOf() = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("AttributionOf() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("AttributionOf() = %q, want %q", got, tc.want)
			}
		})
	}
}
