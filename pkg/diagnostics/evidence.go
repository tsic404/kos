package diagnostics

import (
	"sort"
	"strings"
	"time"
)

// Link names one position of the reference chain C-26 walks. The order of the
// constants is the order the chain gives segments: an extension object sits
// after the node, at the dependency end.
type Link string

const (
	// LinkApplication is the application the chain starts from.
	LinkApplication Link = "application"
	// LinkEndpoint is the endpoint that exposes it (C-43).
	LinkEndpoint Link = "endpoint"
	// LinkRoute is the route object an endpoint owns (C-43).
	LinkRoute Link = "route"
	// LinkBackend is the service the route points at.
	LinkBackend Link = "backend"
	// LinkWorkload is the rendered workload objects (C-32).
	LinkWorkload Link = "workload"
	// LinkNode is the node the workload's pods run on.
	LinkNode Link = "node"
	// LinkExtension is one extension object the application renders (C-19).
	LinkExtension Link = "extension"
)

// Basis separates what the chain observed from what it inferred (C-26).
type Basis string

const (
	// BasisFact is a state read from an object.
	BasisFact Basis = "Fact"
	// BasisInference is a conclusion drawn from several observations.
	BasisInference Basis = "Inference"
)

// ObjectRef identifies one object an evidence entry is about.
type ObjectRef struct {
	// Group is the API group; empty for the core group.
	Group string `json:"group,omitempty"`
	// Version is the API version the object was read in.
	Version string `json:"version,omitempty"`
	// Kind is the object kind.
	Kind string `json:"kind"`
	// Namespace is empty for cluster-scoped objects.
	Namespace string `json:"namespace,omitempty"`
	// Name is the object name.
	Name string `json:"name"`
}

// String renders the reference for messages.
func (r ObjectRef) String() string {
	parts := make([]string, 0, 3)
	if r.Group != "" {
		parts = append(parts, r.Group)
	}
	parts = append(parts, r.Kind)
	if r.Namespace != "" {
		parts = append(parts, r.Namespace)
	}
	parts = append(parts, r.Name)
	return strings.Join(parts, "/")
}

// Evidence is one entry of C-26's evidence shape: conclusion, checked object,
// data source, observation time, raw state, impact and next action, with the
// basis telling fact from inference.
type Evidence struct {
	// Conclusion is what the observation establishes.
	Conclusion string `json:"conclusion"`
	// Object is the object that was checked.
	Object ObjectRef `json:"object"`
	// Source names where the state came from.
	Source string `json:"source"`
	// ObservedAt is when the state was read.
	ObservedAt time.Time `json:"observedAt"`
	// RawState is the observed state or error, without interpretation.
	RawState string `json:"rawState"`
	// Impact is what the state means for the user.
	Impact string `json:"impact"`
	// NextAction is what resolves the state.
	NextAction string `json:"nextAction"`
	// Basis tells whether the entry is observed or inferred (C-26).
	Basis Basis `json:"basis"`
}

// Segment is one link of the chain with its evidence. An unobservable segment
// keeps its place and names why no evidence could be produced (C-26).
type Segment struct {
	// Link is the position of the segment in the chain.
	Link Link `json:"link"`
	// Object is the object the segment is about.
	Object ObjectRef `json:"object"`
	// Observable is false when the segment's adapter is unavailable.
	Observable bool `json:"observable"`
	// Reason explains an unobservable segment.
	Reason string `json:"reason,omitempty"`
	// Evidence is the segment's entries, most specific first.
	Evidence []Evidence `json:"evidence"`
}

// Conclusion names the degradation reason, the object it belongs to and the
// evidence segment it sits in, so one reason points at one object and one
// segment (C-26).
type Conclusion struct {
	// Summary is the reason in one sentence.
	Summary string `json:"summary"`
	// Basis tells whether the reason is observed or inferred (C-26).
	Basis Basis `json:"basis"`
	// Segment is the link the reason sits in.
	Segment Link `json:"segment"`
	// Object is the object the reason points at.
	Object ObjectRef `json:"object"`
	// NextAction is what resolves it.
	NextAction string `json:"nextAction,omitempty"`
}

// Chain is the evidence chain of one application (C-26).
type Chain struct {
	// Application is the application the chain starts from.
	Application ObjectRef `json:"application"`
	// ObservedAt is when the chain was assembled.
	ObservedAt time.Time `json:"observedAt"`
	// Segments is the chain in reference order.
	Segments []Segment `json:"segments"`
	// Conclusion is the degradation reason, or the statement that the path is
	// intact.
	Conclusion Conclusion `json:"conclusion"`
}

// linkOrder is the chain order: the application first, an extension object at
// the dependency end.
var linkOrder = []Link{
	LinkApplication, LinkEndpoint, LinkRoute, LinkBackend, LinkWorkload, LinkNode, LinkExtension,
}

// linkRank places one link in the chain. The segment order and the deepest
// failure both read it, so the conclusion can only point at a segment that
// sits at or after every other failed segment. An unknown link ranks last.
func linkRank(link Link) int {
	for index, candidate := range linkOrder {
		if candidate == link {
			return index
		}
	}
	return len(linkOrder)
}

// sortSegments orders segments by link; within a link the assembly order is
// kept, so a deterministic object order produces a deterministic chain.
func sortSegments(segments []Segment) {
	sort.SliceStable(segments, func(left, right int) bool {
		return linkRank(segments[left].Link) < linkRank(segments[right].Link)
	})
}
