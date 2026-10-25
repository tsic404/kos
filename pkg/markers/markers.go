// Package markers holds the attribution markers of the kernel object model and
// the checks that keep them on their contract carrier (C-02).
//
// The four classes are attribution, decline, release and validity. Every key
// except adopted-at is a label; unknown annotations never change behaviour.
package markers

import (
	"fmt"
	"sort"
)

// Carrier is the metadata field a marker key lives on.
type Carrier string

const (
	// CarrierLabel marks a key carried by metadata.labels.
	CarrierLabel Carrier = "label"
	// CarrierAnnotation marks a key carried by metadata.annotations.
	CarrierAnnotation Carrier = "annotation"
)

// Class groups the marker keys by what they express (C-02).
type Class string

const (
	// ClassAttribution tells whether KOS manages, adopted or ignores an object.
	ClassAttribution Class = "attribution"
	// ClassDecline tells that the user declined to hand an object to KOS.
	ClassDecline Class = "decline"
	// ClassRelease tells that a PVC has been unbound.
	ClassRelease Class = "release"
	// ClassValidity tells that an object or its fact is no longer trustworthy.
	ClassValidity Class = "validity"
)

// Marker keys. The strings are wire format: they are used verbatim, without a
// group prefix, because C-02 names them.
const (
	// ManagedBy marks an object KOS created and manages.
	ManagedBy = "managed-by"
	// OwnerUID names the UID of the object's owner.
	OwnerUID = "owner-uid"
	// AdoptedBy marks an object KOS took over after explicit confirmation.
	AdoptedBy = "adopted-by"
	// AdoptionReason records why an object was adopted.
	AdoptionReason = "adoption-reason"
	// AdoptedAt records when an object was adopted; it is the only annotation
	// marker (C-02).
	AdoptedAt = "adopted-at"
	// Declined marks an object the user declined to hand to KOS.
	Declined = "declined"
	// Released marks a PVC that has been unbound.
	Released = "released"
	// Invalid marks an object whose desired state was rejected.
	Invalid = "invalid"
	// Stale marks a device fact that is no longer refreshed.
	Stale = "stale"
)

// marker describes where a key lives and what it expresses.
type marker struct {
	carrier Carrier
	class   Class
}

var known = map[string]marker{
	ManagedBy:      {carrier: CarrierLabel, class: ClassAttribution},
	OwnerUID:       {carrier: CarrierLabel, class: ClassAttribution},
	AdoptedBy:      {carrier: CarrierLabel, class: ClassAttribution},
	AdoptionReason: {carrier: CarrierLabel, class: ClassAttribution},
	AdoptedAt:      {carrier: CarrierAnnotation, class: ClassAttribution},
	Declined:       {carrier: CarrierLabel, class: ClassDecline},
	Released:       {carrier: CarrierLabel, class: ClassRelease},
	Invalid:        {carrier: CarrierLabel, class: ClassValidity},
	Stale:          {carrier: CarrierLabel, class: ClassValidity},
}

// Keys returns every marker key in sorted order.
func Keys() []string {
	keys := make([]string, 0, len(known))
	for key := range known {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// CarrierOf returns the single carrier a marker key may use; ok is false for a
// key that is not a marker.
func CarrierOf(key string) (carrier Carrier, ok bool) {
	entry, ok := known[key]
	return entry.carrier, ok
}

// ClassOf returns the class a marker key belongs to; ok is false for a key that
// is not a marker.
func ClassOf(key string) (class Class, ok bool) {
	entry, ok := known[key]
	return entry.class, ok
}

// Attribution is the three-state attribution of an object (C-02).
type Attribution string

const (
	// AttributionManaged means metadata.labels carries managed-by.
	AttributionManaged Attribution = "managed"
	// AttributionAdopted means the object was taken over and carries its adoption markers.
	AttributionAdopted Attribution = "adopted"
	// AttributionUnmanaged means no attribution marker is present.
	AttributionUnmanaged Attribution = "unmanaged"
)

// AttributionOf derives the attribution from labels alone.
//
// managed-by and the adoption pair are mutually exclusive: an object is managed
// or adopted, never both. A half-written adoption pair is an error, not a
// default. Unknown labels are ignored.
func AttributionOf(labels map[string]string) (Attribution, error) {
	_, managed := labels[ManagedBy]
	_, adoptedBy := labels[AdoptedBy]
	_, reason := labels[AdoptionReason]

	switch {
	case managed && (adoptedBy || reason):
		adoption := AdoptedBy
		if !adoptedBy {
			adoption = AdoptionReason
		}
		return "", fmt.Errorf("conflicting attribution: %q and %q are both present", ManagedBy, adoption)
	case adoptedBy != reason:
		return "", fmt.Errorf("incomplete adoption: %q and %q must be present together", AdoptedBy, AdoptionReason)
	case adoptedBy:
		return AttributionAdopted, nil
	case managed:
		return AttributionManaged, nil
	default:
		return AttributionUnmanaged, nil
	}
}

// Validate checks carrier placement and attribution completeness.
//
// It reports every violation it finds; unknown keys on either carrier are
// ignored, because no annotation outside C-02 may change behaviour.
func Validate(labels, annotations map[string]string) error {
	var problems []string
	problems = append(problems, carrierViolations(CarrierLabel, labels)...)
	problems = append(problems, carrierViolations(CarrierAnnotation, annotations)...)

	attribution, err := AttributionOf(labels)
	if err != nil {
		problems = append(problems, err.Error())
	}
	if err == nil && attribution == AttributionAdopted {
		if _, ok := annotations[AdoptedAt]; !ok {
			problems = append(problems, fmt.Sprintf("adopted object lacks the %q annotation", AdoptedAt))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("marker validation failed: %v", problems)
	}
	return nil
}

// carrierViolations reports known marker keys that sit on the wrong carrier.
func carrierViolations(carrier Carrier, values map[string]string) []string {
	var violations []string
	for key := range values {
		expected, ok := CarrierOf(key)
		if !ok || expected == carrier {
			continue
		}
		violations = append(violations, fmt.Sprintf("%q belongs on %s, not %s", key, expected, carrier))
	}
	return violations
}
