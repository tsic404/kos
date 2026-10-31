package controlplane

import (
	"context"
	"errors"
	"log/slog"
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
	var filter *EventFilter
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
		filter = &EventFilter{Ref: ref}
		scope = ref.Namespace
	}
	read, failure := (EventReader{
		Source: s.source, Authorizer: s.authorizer, Now: s.now, Log: s.log,
	}).Read(r.Context(), scope, filter)
	if failure != nil {
		writeError(w, failure)
		return
	}
	writeJSON(w, http.StatusOK, read)
}

// EventFilter narrows a read to the events about one subject (C-64).
type EventFilter struct {
	// Ref is the object the read is about.
	Ref ObjectRef
}

// Matches reports whether an event concerns the filtered object. All four
// identity fields compare equal: a subject that names the same kind and name
// under another API group, another namespace, or no apiVersion at all is
// another object, whatever the session may see of it (C-64).
func (f EventFilter) Matches(event Event) bool {
	return event.InvolvedObject == f.Ref
}

// EventReader reads C-64's window over one object source. It is the one
// implementation of the window, the order and the visibility rule, so the API
// handler and a page source cannot report the same event differently.
type EventReader struct {
	// Source reads the events and the objects the visibility rule judges;
	// required.
	Source ObjectSource
	// Authorizer judges whether a session may read an event's subject; nil
	// admits every event.
	Authorizer Authorizer
	// Now supplies the read clock; defaults to time.Now.
	Now func() time.Time
	// Log records the detail a user-facing failure must not carry; nil
	// discards it.
	Log *slog.Logger
}

// Read returns the events of the window that ends at the read, newest first. An
// event the session may not see is absent from the answer; a cluster read that
// failed fails the whole read (C-64).
func (r EventReader) Read(ctx context.Context, namespace string, filter *EventFilter) (EventRead, *Error) {
	if r.Authorizer == nil {
		r.Authorizer = AllowAll{}
	}
	now := r.now()
	observed, err := r.Source.List(ctx, eventClass, namespace)
	if err != nil {
		return EventRead{}, ClusterReadFailure(err, ObjectRef{Kind: "Event", Namespace: namespace}, r.Log)
	}
	cutoff := now.Add(-EventWindow)
	events := make([]Event, 0, len(observed))
	visible := make(map[string]bool)
	for _, item := range observed {
		event, ok := DecodeEvent(item.Object)
		// The window is the hour that ends at the read (C-13): an event the
		// cluster stamped after it cannot be part of what this read observed,
		// so a clock ahead of the control plane's does not put it in front.
		if !ok || event.LastSeen.Before(cutoff) || event.LastSeen.After(now) {
			continue
		}
		if filter != nil && !filter.Matches(event) {
			continue
		}
		if filter == nil {
			shown, failure := r.visible(ctx, event.InvolvedObject, visible)
			if failure != nil {
				return EventRead{}, failure
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
	return EventRead{ObservedAt: now, Window: EventWindow.String(), Events: events}, nil
}

// now returns the read clock.
func (r EventReader) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

// visible applies C-64's rule to one event subject. A refusal hides that event,
// not the whole read; the verdict is cached per subject, so one read resolves
// each subject once instead of re-reading it per event.
func (r EventReader) visible(ctx context.Context, subject ObjectRef, cache map[string]bool) (bool, *Error) {
	key := subject.String()
	if shown, cached := cache[key]; cached {
		return shown, nil
	}
	var object map[string]any
	objectType, known := kernelType(subject.Kind)
	if known && (subject.Group == "" || subject.Group == objectType.Group) {
		observed, err := r.Source.Get(ctx, objectType, subject.Namespace, subject.Name)
		switch {
		case err == nil:
			object = observed.Object
		case apierrors.IsNotFound(err):
			// An event about an object that is gone is judged as an object
			// without an owner.
		default:
			return false, ClusterReadFailure(err, subject, r.Log)
		}
	}
	verb := VerbEventRead
	if known && objectType.Group == api.Group && objectType.Kind == "Application" {
		verb = VerbRead
	}
	_, err := r.Authorizer.Decide(ctx, Request{Verb: verb, Ref: subject, Object: object})
	switch {
	case err == nil:
		cache[key] = true
		return true, nil
	case errors.Is(err, ErrHidden):
		cache[key] = false
		return false, nil
	default:
		if r.Log != nil {
			r.Log.Info("event hidden from session", "object", key, "reason", err)
		}
		cache[key] = false
		return false, nil
	}
}

// DecodeEvent reads one stored event. An event without a subject or without any
// timestamp is not a readable event of the window.
func DecodeEvent(object map[string]any) (Event, bool) {
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
