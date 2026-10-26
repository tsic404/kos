package endpoint

import (
	"sort"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
)

// Conflict kinds the ledger reports (C-44, C-45).
const (
	// ConflictDuplicateKey marks two endpoints holding one key.
	ConflictDuplicateKey = "DuplicateKey"
	// ConflictHostExclusive marks an http.host and an http.path on one host.
	ConflictHostExclusive = "HostExclusive"
	// ConflictTargetMissing marks an endpoint whose application is gone.
	ConflictTargetMissing = "TargetMissing"
)

// SharedMediumNote is what an mdns projection carries: the advertisement is
// visible to every host on the segment (C-43).
const SharedMediumNote = "mDNS 使用共享介质：同网段的任何主机都能发现该服务"

// Entry is one endpoint as the ledger observed it.
type Entry struct {
	// Ref identifies the endpoint.
	Ref write.Ref `json:"ref"`
	// Spec is the endpoint's desired shape.
	Spec v1beta1.EndpointSpec `json:"spec"`
	// Attribution is the managed, adopted or unmanaged state of the object.
	Attribution markers.Attribution `json:"attribution"`
	// TargetMissing reports that the application the endpoint names does not
	// exist in its namespace (C-45).
	TargetMissing bool `json:"targetMissing,omitempty"`
	// Invalid carries a C-43 rejection; an invalid endpoint holds no keys.
	Invalid *FieldError `json:"invalid,omitempty"`
}

// EntryOf describes one endpoint without the reconciler's declaration checks:
// the shape, the attribution and the keys it would hold. A shape or marker the
// kernel rejects is carried as Invalid, which holds no keys; an unreadable
// attribution reads as unmanaged so no cascade deletes an object it cannot
// attribute.
func EntryOf(object *unstructured.Unstructured) Entry {
	entry := Entry{Ref: write.RefOf(object), Attribution: markers.AttributionUnmanaged}
	attribution, err := markers.AttributionOf(object.GetLabels())
	if err != nil {
		entry.Invalid = &FieldError{Field: "metadata.labels", Reason: err.Error()}
	} else {
		entry.Attribution = attribution
	}
	spec, err := SpecOf(object)
	if err != nil {
		entry.Invalid = &FieldError{Field: "spec", Reason: err.Error()}
		return entry
	}
	entry.Spec = spec
	return entry
}

// Keys returns the ledger keys the entry holds; an invalid entry holds none.
func (e Entry) Keys() []Key {
	if e.Invalid != nil {
		return nil
	}
	return KeysOf(e.Spec)
}

// Conflict is one reason an endpoint is not admitted.
type Conflict struct {
	// Kind is one of the conflict kinds above.
	Kind string `json:"kind"`
	// Key is the ledger key or the missing application name.
	Key string `json:"key,omitempty"`
	// With names the other endpoints of the conflict.
	With []write.Ref `json:"with,omitempty"`
}

// Message renders the conflict the way a condition carries it.
func (c Conflict) Message() string {
	switch c.Kind {
	case ConflictDuplicateKey:
		return "duplicate key " + c.Key + " shared with " + joinRefs(c.With)
	case ConflictHostExclusive:
		return "host " + c.Key + " is held by an http.host endpoint " + joinRefs(c.With)
	case ConflictTargetMissing:
		return "target application " + c.Key + " does not exist"
	default:
		return c.Kind + " " + c.Key
	}
}

// Ledger is the endpoint reconciler's in-memory allocation projection (C-44).
// It is derived state: Rebuild recreates it from the stored endpoints, and no
// user write targets it.
type Ledger struct {
	mu      sync.Mutex
	entries map[string]Entry
}

// NewLedger returns an empty ledger.
func NewLedger() *Ledger {
	return &Ledger{entries: map[string]Entry{}}
}

// Put records one endpoint and returns it together with every endpoint whose
// conflict set the observation changed. A shared key and an exclusive host both
// make a peer's admission depend on this entry, so the caller settles the whole
// returned set and one event converges them all (C-06, C-44).
//
// A later observation of the same reference replaces the earlier one, so the
// ledger follows the object's current spec.
func (l *Ledger) Put(entry Entry) []write.Ref {
	l.mu.Lock()
	defer l.mu.Unlock()
	before := conflictSignatures(l.entries)
	l.entries[entry.Ref.Key()] = entry
	after := conflictSignatures(l.entries)
	changed := append(changedEntries(before, after, l.entries), entry.Ref)
	return sortRefs(changed)
}

// Remove releases every key the referenced endpoint held and returns the
// identifiers whose conflict set the release changed, so a deletion settles the
// surviving holders instead of leaving a stale condition behind (C-06, C-44).
func (l *Ledger) Remove(ref write.Ref) []write.Ref {
	l.mu.Lock()
	defer l.mu.Unlock()
	before := conflictSignatures(l.entries)
	delete(l.entries, ref.Key())
	after := conflictSignatures(l.entries)
	return sortRefs(changedEntries(before, after, l.entries))
}

// Replace makes entries the whole ledger, so a rebuild starts from the stored
// objects alone without exposing a half-empty projection.
func (l *Ledger) Replace(entries []Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	replaced := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		replaced[entry.Ref.Key()] = entry
	}
	l.entries = replaced
}

// Entry returns the observation of one endpoint.
func (l *Ledger) Entry(ref write.Ref) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[ref.Key()]
	return entry, ok
}

// Refs returns every reference in the ledger, ordered.
func (l *Ledger) Refs() []write.Ref {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := orderedEntries(l.entries)
	refs := make([]write.Ref, 0, len(entries))
	for _, entry := range entries {
		refs = append(refs, entry.Ref)
	}
	return refs
}

// Entries returns every entry ordered by reference.
func (l *Ledger) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return orderedEntries(l.entries)
}

// Conflicts returns what keeps one endpoint from being admitted, ordered so two
// observations produce the same list.
func (l *Ledger) Conflicts(ref write.Ref) []Conflict {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[ref.Key()]
	if !ok {
		return nil
	}
	return conflictsOf(entry, l.entries)
}

// Projection is one ledger row: the endpoint, the keys it holds and what blocks
// it. It is the read model the network page's port ledger comes from (C-50).
type Projection struct {
	Entry
	// Keys are the allocated keys of this endpoint.
	Keys []Key `json:"keys"`
	// Conflicts are the reasons it is not admitted.
	Conflicts []Conflict `json:"conflicts"`
	// SharedMedium tells that the shape advertises on a shared medium.
	SharedMedium bool `json:"sharedMedium,omitempty"`
	// Note carries the security impact the panel must show (C-43).
	Note string `json:"note,omitempty"`
}

// Projection returns every row ordered by reference.
func (l *Ledger) Projection() []Projection {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := orderedEntries(l.entries)
	projections := make([]Projection, 0, len(entries))
	for _, entry := range entries {
		row := Projection{
			Entry:     entry,
			Keys:      entry.Keys(),
			Conflicts: conflictsOf(entry, l.entries),
		}
		if entry.Spec.Kind == v1beta1.EndpointKindMDNS {
			row.SharedMedium = true
			row.Note = SharedMediumNote
		}
		projections = append(projections, row)
	}
	return projections
}

// conflictSignatures renders the conflict list of every entry, so a mutation
// can name exactly the entries whose admission it moved.
func conflictSignatures(entries map[string]Entry) map[string]string {
	signatures := make(map[string]string, len(entries))
	for key, entry := range entries {
		signatures[key] = conflictSignature(conflictsOf(entry, entries))
	}
	return signatures
}

// conflictSignature renders one conflict list; conflictsOf already orders it.
func conflictSignature(conflicts []Conflict) string {
	var signature strings.Builder
	for _, conflict := range conflicts {
		signature.WriteString(conflict.Kind)
		signature.WriteString("|")
		signature.WriteString(conflict.Key)
		for _, ref := range conflict.With {
			signature.WriteString("|")
			signature.WriteString(ref.Key())
		}
		signature.WriteString(";")
	}
	return signature.String()
}

// changedEntries names the entries whose signature moved; the entries map
// supplies their references. Callers hold the lock.
func changedEntries(before, after map[string]string, entries map[string]Entry) []write.Ref {
	refs := make([]write.Ref, 0, len(after))
	for key, signature := range after {
		if before[key] == signature {
			continue
		}
		refs = append(refs, entries[key].Ref)
	}
	return refs
}

// orderedEntries returns the entries of a map ordered by reference; callers
// hold the lock.
func orderedEntries(entries map[string]Entry) []Entry {
	ordered := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		ordered = append(ordered, entry)
	}
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].Ref.Key() < ordered[right].Ref.Key()
	})
	return ordered
}

// conflictsOf lists what blocks one entry against the whole ledger.
func conflictsOf(entry Entry, entries map[string]Entry) []Conflict {
	if entry.Invalid != nil {
		return nil
	}
	host := CanonicalHost(entry.Spec.Host)
	held := KeysOf(entry.Spec)
	byKey := make(map[string][]write.Ref, len(held))
	exclusive := make([]write.Ref, 0)
	for _, other := range entries {
		if other.Ref.Key() == entry.Ref.Key() || other.Invalid != nil {
			continue
		}
		for _, key := range KeysOf(other.Spec) {
			for _, mine := range held {
				if key == mine {
					byKey[mine.String()] = append(byKey[mine.String()], other.Ref)
				}
			}
		}
		// C-44 makes the two HTTP shapes exclusive on one host in both
		// directions: a host that carries a hostname endpoint carries no
		// path endpoint, and vice versa.
		if host != "" && otherHostConflicts(entry.Spec.Kind, other.Spec.Kind, host, CanonicalHost(other.Spec.Host)) {
			exclusive = append(exclusive, other.Ref)
		}
	}
	conflicts := make([]Conflict, 0, len(byKey)+2)
	for key, refs := range byKey {
		conflicts = append(conflicts, Conflict{Kind: ConflictDuplicateKey, Key: key, With: sortRefs(refs)})
	}
	if len(exclusive) > 0 {
		conflicts = append(conflicts, Conflict{Kind: ConflictHostExclusive, Key: host, With: sortRefs(exclusive)})
	}
	if entry.TargetMissing {
		target := write.Ref{Group: api.Group, Version: v1beta1.GroupVersion.Version, Kind: "Application", Namespace: entry.Ref.Namespace, Name: entry.Spec.TargetApp}
		conflicts = append(conflicts, Conflict{Kind: ConflictTargetMissing, Key: target.Namespace + "/" + target.Name})
	}
	sort.Slice(conflicts, func(left, right int) bool {
		if conflicts[left].Kind != conflicts[right].Kind {
			return conflicts[left].Kind < conflicts[right].Kind
		}
		return conflicts[left].Key < conflicts[right].Key
	})
	return conflicts
}

// otherHostConflicts reports whether two shapes contend for one host.
func otherHostConflicts(kind, otherKind v1beta1.EndpointKind, host, otherHost string) bool {
	if host != otherHost || host == "" {
		return false
	}
	return (kind == v1beta1.EndpointKindHTTPPath && otherKind == v1beta1.EndpointKindHTTPHost) ||
		(kind == v1beta1.EndpointKindHTTPHost && otherKind == v1beta1.EndpointKindHTTPPath)
}

// sortRefs orders references for a deterministic message.
func sortRefs(refs []write.Ref) []write.Ref {
	sorted := make([]write.Ref, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if seen[ref.Key()] {
			continue
		}
		seen[ref.Key()] = true
		sorted = append(sorted, ref)
	}
	sort.Slice(sorted, func(left, right int) bool {
		return sorted[left].Key() < sorted[right].Key()
	})
	return sorted
}

// joinRefs names the other objects of a conflict.
func joinRefs(refs []write.Ref) string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		name := ref.Name
		if ref.Namespace != "" {
			name = ref.Namespace + "/" + name
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}
