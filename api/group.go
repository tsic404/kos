// Package api holds the identity of the KOS API group and its closed kind set.
package api

import "sort"

// Group is the API group of the kernel objects.
//
// Extension object classes must live in their own group (C-19); a kind added
// here is a framework change (C-01).
const Group = "kos.tsic.top"

// ConditionReconciled is the condition every object with a status writer
// carries; failure conditions are separate (C-07).
const ConditionReconciled = "Reconciled"

// SystemNamespace holds the objects KOS keeps for itself: the control plane,
// action records and adoption snapshots (C-04, C-35, C-54).
const SystemNamespace = "kos-system"

// Scope is the reach of an object kind.
type Scope string

const (
	// ScopeCluster marks a kind that exists once per cluster.
	ScopeCluster Scope = "Cluster"
	// ScopeNamespaced marks a kind that exists inside a namespace.
	ScopeNamespaced Scope = "Namespaced"
)

// kindInfo describes one kernel kind. hasStatus follows C-07: the eight kinds
// with a status writer carry Reconciled conditions, the other two carry none.
type kindInfo struct {
	scope     Scope
	hasStatus bool
}

// kinds is the closed kernel object model (C-01). Membership is not a
// configuration value: the ten entries are the whole model.
var kinds = map[string]kindInfo{
	"Application":           {scope: ScopeNamespaced, hasStatus: true},
	"ApplicationDefinition": {scope: ScopeCluster, hasStatus: false},
	"Volume":                {scope: ScopeNamespaced, hasStatus: true},
	"Endpoint":              {scope: ScopeNamespaced, hasStatus: true},
	"Provider":              {scope: ScopeCluster, hasStatus: true},
	"SystemComponent":       {scope: ScopeCluster, hasStatus: true},
	"BackupPolicy":          {scope: ScopeNamespaced, hasStatus: true},
	"RestoreJob":            {scope: ScopeNamespaced, hasStatus: true},
	"NodeDevice":            {scope: ScopeCluster, hasStatus: false},
	"VolumeMigration":       {scope: ScopeNamespaced, hasStatus: true},
}

// Kinds returns the ten kernel kinds in sorted order.
func Kinds() []string {
	names := make([]string, 0, len(kinds))
	for name := range kinds {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ScopeOf reports the scope of a kernel kind; ok is false for an unknown kind.
func ScopeOf(kind string) (scope Scope, ok bool) {
	info, ok := kinds[kind]
	return info.scope, ok
}

// HasStatus reports whether the kind has a status writer and therefore
// carries Reconciled conditions (C-07).
func HasStatus(kind string) (has bool, ok bool) {
	info, ok := kinds[kind]
	return info.hasStatus, ok
}
