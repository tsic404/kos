package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// eventAuthorizer allows every request, remembers what it judged and hides the
// subjects a case names.
type eventAuthorizer struct {
	hidden   map[ObjectRef]bool
	requests []Request
}

// eventTrail collects the audit records the route writes.
type eventTrail struct{ records []AuditRecord }

// Record implements Auditor.
func (t *eventTrail) Record(_ context.Context, record AuditRecord) error {
	t.records = append(t.records, record)
	return nil
}

// Decide implements Authorizer.
func (a *eventAuthorizer) Decide(_ context.Context, request Request) (Grant, error) {
	a.requests = append(a.requests, request)
	if a.hidden[request.Ref] {
		return Grant{}, ErrHidden
	}
	return Grant{}, nil
}

// verbs returns the verbs the authorizer judged, in order.
func (a *eventAuthorizer) verbs() []Verb {
	verbs := make([]Verb, 0, len(a.requests))
	for _, request := range a.requests {
		verbs = append(verbs, request.Verb)
	}
	return verbs
}

// eventsServer serves one API over a fake source.
func eventsServer(t *testing.T, source ObjectSource, authorizer Authorizer) *httptest.Server {
	t.Helper()
	return tunedEventsServer(t, func(config *Config) {
		config.Source = source
		config.Authorizer = authorizer
	})
}

// tunedEventsServer serves one API whose configuration a case completes.
func tunedEventsServer(t *testing.T, tune func(*Config)) *httptest.Server {
	t.Helper()
	config := Config{
		Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		Now:     func() time.Time { return fixedReadTime },
	}
	tune(&config)
	server, err := New(config)
	if err != nil {
		t.Fatalf("build control plane: %v", err)
	}
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer
}

// readEvents calls the event route and decodes the answer.
func readEvents(t *testing.T, server *httptest.Server, path string) (int, map[string]any) {
	t.Helper()
	response, err := http.Get(server.URL + path)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	defer response.Body.Close()
	body := map[string]any{}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode the event read: %v", err)
	}
	return response.StatusCode, body
}

// storedEvent is one event about one object, stamped at lastSeen.
func storedEvent(name, kind, namespace, objectName string, lastSeen time.Time) map[string]any {
	involved := map[string]any{"kind": kind, "name": objectName}
	if namespace != "" {
		involved["namespace"] = namespace
	}
	switch kind {
	case "Application", "Volume", "Endpoint":
		involved["apiVersion"] = "kos.tsic.top/v1beta1"
	case "Pod":
		involved["apiVersion"] = "v1"
	}
	return map[string]any{
		"apiVersion":     "v1",
		"kind":           "Event",
		"metadata":       map[string]any{"name": name, "namespace": namespace},
		"involvedObject": involved,
		"type":           "Warning",
		"reason":         "BackendUnavailable",
		"message":        "no ready backend",
		"count":          int64(3),
		"firstTimestamp": lastSeen.Add(-time.Minute).Format(time.RFC3339Nano),
		"lastTimestamp":  lastSeen.Format(time.RFC3339Nano),
	}
}

// eventWorld is the object set the event cases read over.
func eventWorld(events ...map[string]any) []map[string]any {
	objects := []map[string]any{
		logApplication("shop", renderedWorkload("Deployment", "shop")),
		logApplication("blog", renderedWorkload("Deployment", "blog")),
		{
			"apiVersion": "kos.tsic.top/v1beta1",
			"kind":       "Volume",
			"metadata":   map[string]any{"name": "data", "namespace": "default"},
			"spec":       map[string]any{"owner": "alice"},
		},
	}
	return append(objects, events...)
}

// TestEventsUseTheOneHourWindowNewestFirst pins C-64's event window and order:
// only the last hour is readable, and the newest event comes first.
func TestEventsUseTheOneHourWindowNewestFirst(t *testing.T) {
	world := eventWorld(
		storedEvent("shop-old", "Application", "default", "shop", fixedReadTime.Add(-2*time.Hour)),
		storedEvent("shop-recent", "Application", "default", "shop", fixedReadTime.Add(-5*time.Minute)),
		storedEvent("shop-older", "Application", "default", "shop", fixedReadTime.Add(-50*time.Minute)),
	)
	server := eventsServer(t, sourceWorld(world), AllowAll{})

	status, body := readEvents(t, server, "/api/v1/events?namespace=default")
	if status != http.StatusOK {
		t.Fatalf("read events = %d %v", status, body)
	}
	if body["window"] != "1h0m0s" {
		t.Fatalf("window = %v, want the one-hour window", body["window"])
	}
	if body["observedAt"] != fixedReadTime.Format(time.RFC3339Nano) {
		t.Fatalf("observedAt = %v, want the read time", body["observedAt"])
	}
	names := eventNames(t, body)
	if len(names) != 2 || names[0] != "shop-recent" || names[1] != "shop-older" {
		t.Fatalf("events = %v, want the two in-window events newest first", names)
	}
}

// TestEventsDropWhatTheWindowDoesNotCover pins the window at its far end: an
// event the cluster stamped after the read is not part of the hour the read
// observed, so it is not returned even though it is not older than the window.
func TestEventsDropWhatTheWindowDoesNotCover(t *testing.T) {
	world := eventWorld(
		storedEvent("shop-recent", "Application", "default", "shop", fixedReadTime.Add(-5*time.Minute)),
		storedEvent("shop-ahead", "Application", "default", "shop", fixedReadTime.Add(time.Minute)),
	)
	server := eventsServer(t, sourceWorld(world), AllowAll{})

	status, body := readEvents(t, server, "/api/v1/events?namespace=default")
	if status != http.StatusOK {
		t.Fatalf("read events = %d %v", status, body)
	}
	names := eventNames(t, body)
	if len(names) != 1 || names[0] != "shop-recent" {
		t.Fatalf("events = %v, want only the event the read observed", names)
	}
}

// TestEventsFilterByObject pins C-64's per-object filter: a read named for one
// application returns that application's events only.
func TestEventsFilterByObject(t *testing.T) {
	world := eventWorld(
		storedEvent("shop-recent", "Application", "default", "shop", fixedReadTime.Add(-5*time.Minute)),
		storedEvent("blog-recent", "Application", "default", "blog", fixedReadTime.Add(-10*time.Minute)),
	)
	server := eventsServer(t, sourceWorld(world), AllowAll{})

	status, body := readEvents(t, server, "/api/v1/events?namespace=default&kind=Application&name=shop")
	if status != http.StatusOK {
		t.Fatalf("read events = %d %v", status, body)
	}
	names := eventNames(t, body)
	if len(names) != 1 || names[0] != "shop-recent" {
		t.Fatalf("events = %v, want only the shop event", names)
	}
}

// TestEventFilterHoldsObjectIdentity pins C-64's per-object filter at the whole
// identity of the object: an event that names the same kind and name under
// another API group, in another namespace, or without an apiVersion concerns
// another object and stays out of the read.
func TestEventFilterHoldsObjectIdentity(t *testing.T) {
	matching := storedEvent("shop-recent", "Application", "default", "shop", fixedReadTime.Add(-5*time.Minute))
	otherGroup := storedEvent("shop-other-group", "Application", "default", "shop", fixedReadTime.Add(-10*time.Minute))
	otherGroup["involvedObject"].(map[string]any)["apiVersion"] = "example.com/v1beta1"
	unversioned := storedEvent("shop-unversioned", "Application", "default", "shop", fixedReadTime.Add(-15*time.Minute))
	delete(unversioned["involvedObject"].(map[string]any), "apiVersion")
	otherNamespace := storedEvent("shop-other-namespace", "Application", "default", "shop", fixedReadTime.Add(-20*time.Minute))
	otherNamespace["involvedObject"].(map[string]any)["namespace"] = "team-b"
	server := eventsServer(t, sourceWorld(eventWorld(matching, otherGroup, unversioned, otherNamespace)), AllowAll{})

	status, body := readEvents(t, server, "/api/v1/events?namespace=default&kind=Application&name=shop")
	if status != http.StatusOK {
		t.Fatalf("read events = %d %v", status, body)
	}
	names := eventNames(t, body)
	if len(names) != 1 || names[0] != "shop-recent" {
		t.Fatalf("events = %v, want only the event of the filtered object", names)
	}
}

// TestEventFilterHoldsClusterScope pins the same filter for a cluster-scoped
// object, where the read has no namespace of its own to compare: an event that
// names one for a cluster-scoped subject concerns another object.
func TestEventFilterHoldsClusterScope(t *testing.T) {
	provider := map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Provider",
		"metadata":   map[string]any{"name": "cluster"},
	}
	matching := storedEvent("provider-recent", "Provider", "default", "cluster", fixedReadTime.Add(-5*time.Minute))
	matching["involvedObject"].(map[string]any)["apiVersion"] = "kos.tsic.top/v1beta1"
	delete(matching["involvedObject"].(map[string]any), "namespace")
	namespaced := storedEvent("provider-namespaced", "Provider", "default", "cluster", fixedReadTime.Add(-10*time.Minute))
	namespaced["involvedObject"].(map[string]any)["apiVersion"] = "kos.tsic.top/v1beta1"
	namespaced["involvedObject"].(map[string]any)["namespace"] = "default"
	world := append(eventWorld(matching, namespaced), provider)
	server := eventsServer(t, sourceWorld(world), AllowAll{})

	status, body := readEvents(t, server, "/api/v1/events?kind=Provider&name=cluster")
	if status != http.StatusOK {
		t.Fatalf("read events = %d %v", status, body)
	}
	shown := eventNames(t, body)
	if len(shown) != 1 || shown[0] != "provider-recent" {
		t.Fatalf("events = %v, want only the cluster-scoped subject's event", shown)
	}
}

// TestEventsDropWhatTheSessionMayNotSee pins C-64's visibility: the unfiltered
// read of the overview drops an application the session cannot see, and it drops
// a non-application event the event rule hides.
func TestEventsDropWhatTheSessionMayNotSee(t *testing.T) {
	shopRef := ObjectRef{Group: "kos.tsic.top", Kind: "Application", Namespace: "default", Name: "shop"}
	blobRef := ObjectRef{Group: "kos.tsic.top", Kind: "Volume", Namespace: "default", Name: "data"}
	authorizer := &eventAuthorizer{hidden: map[ObjectRef]bool{shopRef: true, blobRef: true}}
	world := eventWorld(
		storedEvent("shop-recent", "Application", "default", "shop", fixedReadTime.Add(-5*time.Minute)),
		storedEvent("blog-recent", "Application", "default", "blog", fixedReadTime.Add(-10*time.Minute)),
		storedEvent("volume-recent", "Volume", "default", "data", fixedReadTime.Add(-15*time.Minute)),
	)
	server := eventsServer(t, sourceWorld(world), authorizer)

	status, body := readEvents(t, server, "/api/v1/events?namespace=default")
	if status != http.StatusOK {
		t.Fatalf("read events = %d %v", status, body)
	}
	names := eventNames(t, body)
	if len(names) != 1 || names[0] != "blog-recent" {
		t.Fatalf("events = %v, want only the visible application's event", names)
	}
}

// TestNonApplicationEventsAreJudgedByTheEventVerb pins the seam: an object
// other than an application is judged under C-64's event verb, and a hidden
// subject answers as absent.
func TestNonApplicationEventsAreJudgedByTheEventVerb(t *testing.T) {
	volumeRef := ObjectRef{Group: "kos.tsic.top", Kind: "Volume", Namespace: "default", Name: "data"}
	authorizer := &eventAuthorizer{hidden: map[ObjectRef]bool{volumeRef: true}}
	world := eventWorld(storedEvent("volume-recent", "Volume", "default", "data", fixedReadTime.Add(-15*time.Minute)))
	server := eventsServer(t, sourceWorld(world), authorizer)

	status, body := readEvents(t, server, "/api/v1/events?namespace=default&kind=Volume&name=data")
	if status != http.StatusNotFound {
		t.Fatalf("hidden subject = %d %v, want 404", status, body)
	}
	verbs := authorizer.verbs()
	if len(verbs) == 0 || verbs[len(verbs)-1] != VerbEventRead {
		t.Fatalf("judged verbs = %v, want the event verb", verbs)
	}
}

// TestRefusedEventReadRecordsNothing pins C-14's record to the writes: an event
// read refused for a subject the session does not own answers as absent and
// leaves no audit row, which is what the unfiltered read of the same session
// already does.
func TestRefusedEventReadRecordsNothing(t *testing.T) {
	volumeRef := ObjectRef{Group: "kos.tsic.top", Kind: "Volume", Namespace: "default", Name: "data"}
	trail := &eventTrail{}
	world := eventWorld(storedEvent("volume-recent", "Volume", "default", "data", fixedReadTime.Add(-15*time.Minute)))
	server := tunedEventsServer(t, func(config *Config) {
		config.Source = sourceWorld(world)
		config.Authorizer = &eventAuthorizer{hidden: map[ObjectRef]bool{volumeRef: true}}
		config.Auditor = trail
	})

	status, body := readEvents(t, server, "/api/v1/events?namespace=default&kind=Volume&name=data")
	if status != http.StatusNotFound {
		t.Fatalf("hidden subject = %d %v, want 404", status, body)
	}
	if len(trail.records) != 0 {
		t.Fatalf("audit = %+v, want no record for a refused read", trail.records)
	}
}

// TestEventsNameTheClusterAsTheReason pins C-64's failure semantics: an
// unreachable API and an unusable credential both answer with a reason instead
// of a successful empty list.
func TestEventsNameTheClusterAsTheReason(t *testing.T) {
	source := sourceWorld(eventWorld())
	source.listErr = errors.New("dial tcp 10.43.0.1:443: connect: connection refused")
	server := eventsServer(t, source, AllowAll{})
	status, body := readEvents(t, server, "/api/v1/events?namespace=default")
	if status != http.StatusBadGateway {
		t.Fatalf("unreachable read = %d %v, want 502", status, body)
	}
	errorBody, _ := body["error"].(map[string]any)
	if errorBody["code"] != CodeClusterUnavailable {
		t.Fatalf("code = %v, want %s", errorBody["code"], CodeClusterUnavailable)
	}
	if _, empty := body["events"]; empty {
		t.Fatalf("an unreachable read answered with a result: %v", body)
	}

	expired := sourceWorld(eventWorld())
	expired.listErr = apierrors.NewUnauthorized("the token has expired")
	server = eventsServer(t, expired, AllowAll{})
	status, body = readEvents(t, server, "/api/v1/events?namespace=default")
	if status != http.StatusBadGateway {
		t.Fatalf("credential read = %d %v, want 502", status, body)
	}
	errorBody, _ = body["error"].(map[string]any)
	if errorBody["code"] != CodeClusterCredentialsInvalid {
		t.Fatalf("code = %v, want %s", errorBody["code"], CodeClusterCredentialsInvalid)
	}
}

// TestDecodeEventFallsBackToTheEventTime pins the timestamp fields the window
// reads: an event that carries only eventTime is still readable.
func TestDecodeEventFallsBackToTheEventTime(t *testing.T) {
	when := fixedReadTime.Add(-time.Minute)
	object := map[string]any{
		"metadata": map[string]any{"name": "only-event-time", "namespace": "default"},
		"involvedObject": map[string]any{
			"apiVersion": "v1", "kind": "Pod", "namespace": "default", "name": "shop-0",
		},
		"eventTime": when.Format(time.RFC3339Nano),
	}
	event, ok := decodeEvent(object)
	if !ok {
		t.Fatal("an event with only eventTime was dropped")
	}
	if !event.LastSeen.Equal(when) || event.InvolvedObject.Name != "shop-0" || event.InvolvedObject.Group != "" {
		t.Fatalf("event = %+v", event)
	}
	if event.FirstSeen != nil {
		t.Fatalf("firstSeen = %v, want none", event.FirstSeen)
	}
}

// TestEventsRejectHalfAnObjectFilter keeps a name without a kind from reading as
// a filter that means nothing.
func TestEventsRejectHalfAnObjectFilter(t *testing.T) {
	server := eventsServer(t, sourceWorld(eventWorld()), AllowAll{})
	status, body := readEvents(t, server, "/api/v1/events?name=shop")
	if status != http.StatusBadRequest {
		t.Fatalf("half a filter = %d %v, want 400", status, body)
	}
	errorBody, _ := body["error"].(map[string]any)
	if errorBody["code"] != CodeInvalidField {
		t.Fatalf("code = %v, want %s", errorBody["code"], CodeInvalidField)
	}
}

// eventNames reads the event names of a response in order.
func eventNames(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["events"].([]any)
	if !ok {
		t.Fatalf("response has no events: %v", body)
	}
	names := make([]string, 0, len(raw))
	for _, entry := range raw {
		event, _ := entry.(map[string]any)
		if name, _ := event["name"].(string); name != "" {
			names = append(names, name)
		}
	}
	return names
}
