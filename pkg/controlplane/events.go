package controlplane

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/tsic404/kos/api"
)

// EventWindow is the window C-64 fixes for every event read: events older than
// one hour are not readable.
const EventWindow = time.Hour

// EventRead is one recent-event read (C-64): events of the window, newest
// first, read when the request runs.
type EventRead struct {
	// ObservedAt is when the read ran (C-13).
	ObservedAt time.Time `json:"observedAt"`
	// Window is the read window.
	Window string `json:"window"`
	// Events are the readable events, newest first.
	Events []Event `json:"events"`
}

// Event is one cluster event as the panel shows it.
type Event struct {
	// Name is the event's name.
	Name string `json:"name"`
	// Namespace is where the event is stored.
	Namespace string `json:"namespace,omitempty"`
	// Type is the event type, Normal or Warning.
	Type string `json:"type"`
	// Reason is the short machine-readable cause.
	Reason string `json:"reason"`
	// Message is the human-readable detail.
	Message string `json:"message"`
	// Count is how many times the event occurred.
	Count int64 `json:"count"`
	// FirstSeen is when the event was first observed; absent when the cluster
	// recorded none.
	FirstSeen *time.Time `json:"firstSeen,omitempty"`
	// LastSeen is the event's time, which is what the window and the order read.
	LastSeen time.Time `json:"lastSeen"`
	// InvolvedObject is the object the event is about.
	InvolvedObject ObjectRef `json:"involvedObject"`
}

// eventClass is the class events are read from.
var eventClass = ObjectType{Version: "v1", Kind: "Event", Resource: "events", Namespaced: true}

// handleEvents reads the recent events of C-64: the window is fixed at one hour,
// the order is newest first, and everything read passes the visibility rule the
// event's subject carries.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	namespace, kind, name := query.Get("namespace"), query.Get("kind"), query.Get("name")
	if (kind == "") != (name == "") {
		writeError(w, invalidField("kind 与 name 必须同时给出"))
		return
	}
	scope := namespace
	var filter *eventFilter
	if kind != "" {
		objectType, failure := resolveType(kind, query.Get("apiVersion"), s.extensions)
		if failure != nil {
			writeError(w, withIdentity(failure, namespace, name))
			return
		}
		if objectType.Namespaced && namespace == "" {
			writeError(w, invalidField("命名空间作用域对象必须给出 namespace 参数"))
			return
		}
		if !objectType.Namespaced {
			namespace = ""
		}
		ref := ObjectRef{Group: objectType.Group, Kind: objectType.Kind, Namespace: namespace, Name: name}
		observed, err := s.source.Get(r.Context(), objectType, namespace, name)
		if err != nil {
			writeError(w, s.clusterReadFailure(err, ref))
			return
		}
		// An application's events follow C-59; every other object's events carry
		// C-64's own rule, which the seam decides under its verb.
		verb := VerbEventRead
		if objectType.Group == api.Group && objectType.Kind == "Application" {
			verb = VerbRead
		}
		if _, failure := s.authorize(r.Context(), verb, ref, observed.Object, nil); failure != nil {
			writeError(w, failure)
			return
		}
		filter = &eventFilter{ref: ref}
		scope = ref.Namespace
	}
	// The read time is taken before the cluster reads the projection names it
	// at (C-13), so it stays the instant the read started however many subject
	// lookups the visibility rule below needs.
	now := s.now()
	observed, err := s.source.List(r.Context(), eventClass, scope)
	if err != nil {
		writeError(w, s.clusterReadFailure(err, ObjectRef{Kind: "Event", Namespace: scope}))
		return
	}
	cutoff := now.Add(-EventWindow)
	events := make([]Event, 0, len(observed))
	visible := make(map[string]bool)
	for _, item := range observed {
		event, ok := decodeEvent(item.Object)
		// The window is the hour that ends at the read (C-13): an event the
		// cluster stamped after it cannot be part of what this read observed,
		// so a clock ahead of the control plane's does not put it in front.
		if !ok || event.LastSeen.Before(cutoff) || event.LastSeen.After(now) {
			continue
		}
		if filter != nil && !filter.matches(event) {
			continue
		}
		if filter == nil {
			shown, failure := s.eventVisible(r.Context(), event.InvolvedObject, visible)
			if failure != nil {
				writeError(w, failure)
				return
			}
			if !shown {
				continue
			}
		}
		events = append(events, event)
	}
	sort.SliceStable(events, func(left, right int) bool {
		if !events[left].LastSeen.Equal(events[right].LastSeen) {
			return events[left].LastSeen.After(events[right].LastSeen)
		}
		return events[left].Name < events[right].Name
	})
	writeJSON(w, http.StatusOK, EventRead{ObservedAt: now, Window: EventWindow.String(), Events: events})
}

// eventFilter narrows a read to the events about one object.
type eventFilter struct {
	ref ObjectRef
}

// matches reports whether an event concerns the filtered object. All four
// identity fields compare equal: a subject that names the same kind and name
// under another API group, another namespace, or no apiVersion at all is
// another object, whatever the session may see of it (C-64).
func (f *eventFilter) matches(event Event) bool {
	return event.InvolvedObject == f.ref
}

// eventVisible applies C-64's visibility rule to one event subject. A refusal
// hides that event, not the whole read: the read spans many objects, so a
// session that may not see one still reads the rest. Only a cluster read that
// failed fails the request.
func (s *Server) eventVisible(ctx context.Context, subject ObjectRef, cache map[string]bool) (bool, *Error) {
	key := subject.String()
	if shown, cached := cache[key]; cached {
		return shown, nil
	}
	var object map[string]any
	objectType, known := kernelType(subject.Kind)
	if known && (subject.Group == "" || subject.Group == objectType.Group) {
		observed, err := s.source.Get(ctx, objectType, subject.Namespace, subject.Name)
		switch {
		case err == nil:
			object = observed.Object
		case apierrors.IsNotFound(err):
			// An event about an object that is gone is judged as an object
			// without an owner.
		default:
			return false, s.clusterReadFailure(err, subject)
		}
	}
	verb := VerbEventRead
	if known && objectType.Group == api.Group && objectType.Kind == "Application" {
		verb = VerbRead
	}
	_, err := s.authorizer.Decide(ctx, Request{Verb: verb, Ref: subject, Object: object})
	switch {
	case err == nil:
		cache[key] = true
		return true, nil
	case errors.Is(err, ErrHidden):
		cache[key] = false
		return false, nil
	default:
		s.log.Info("event hidden from session", "object", key, "reason", err)
		cache[key] = false
		return false, nil
	}
}

// decodeEvent reads one stored event. An event without a subject or without any
// timestamp is not a readable event of the window.
func decodeEvent(object map[string]any) (Event, bool) {
	kind := nestedText(object, "involvedObject", "kind")
	if kind == "" || nestedText(object, "involvedObject", "name") == "" {
		return Event{}, false
	}
	lastSeen := eventTimestamp(object, "lastTimestamp")
	if lastSeen.IsZero() {
		lastSeen = eventTimestamp(object, "eventTime")
	}
	if lastSeen.IsZero() {
		lastSeen = eventTimestamp(object, "firstTimestamp")
	}
	if lastSeen.IsZero() {
		lastSeen = eventTimestamp(object, "metadata", "creationTimestamp")
	}
	if lastSeen.IsZero() {
		return Event{}, false
	}
	group := eventSubjectGroup(nestedText(object, "involvedObject", "apiVersion"))
	event := Event{
		Name:      nestedText(object, "metadata", "name"),
		Namespace: nestedText(object, "metadata", "namespace"),
		Type:      nestedText(object, "type"),
		Reason:    nestedText(object, "reason"),
		Message:   nestedText(object, "message"),
		Count:     eventCount(object),
		LastSeen:  lastSeen,
		InvolvedObject: ObjectRef{
			Group:     group,
			Kind:      kind,
			Namespace: nestedText(object, "involvedObject", "namespace"),
			Name:      nestedText(object, "involvedObject", "name"),
		},
	}
	if first := eventTimestamp(object, "firstTimestamp"); !first.IsZero() {
		event.FirstSeen = &first
	}
	return event, true
}

// eventTimestamp reads one timestamp the event stores as an RFC 3339 string.
func eventTimestamp(object map[string]any, path ...string) time.Time {
	raw := nestedText(object, path...)
	if raw == "" {
		return time.Time{}
	}
	stamp, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return stamp
}

// eventCount reads the repeat count of one event.
func eventCount(object map[string]any) int64 {
	switch typed := object["count"].(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(math.Round(typed))
	}
	return 0
}

// eventSubjectGroup reads the API group an involved object's apiVersion names;
// a core version names none.
func eventSubjectGroup(apiVersion string) string {
	if group, _, ok := splitAPIVersion(apiVersion); ok {
		return group
	}
	return ""
}
