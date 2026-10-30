package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
)

// TestMaskSensitiveReplacesMarkedValues covers C-31/C-32: a config value the
// definition marks sensitive is masked before a projection leaves the API, and
// the stored object keeps its value.
func TestMaskSensitiveReplacesMarkedValues(t *testing.T) {
	definition := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "ApplicationDefinition",
		"metadata":   map[string]any{"name": "db"},
		"spec": map[string]any{"config": []any{
			map[string]any{"name": "password", "type": "string", "sensitive": true},
			map[string]any{"name": "user", "type": "string"},
		}},
	}}
	server := &Server{dynamic: &writeRecorder{current: definition}}
	object := map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "one", "namespace": "default"},
		"spec": map[string]any{
			"definitionRef": "db",
			"config":        map[string]any{"password": "hunter2", "user": "app"},
		},
	}

	masked, failure := server.maskSensitive(context.Background(), "Application", object)
	if failure != nil {
		t.Fatalf("mask: %v", failure)
	}
	config, _, _ := unstructured.NestedMap(masked, "spec", "config")
	if config["password"] != "***" {
		t.Fatalf("sensitive value = %v, want the mask", config["password"])
	}
	if config["user"] != "app" {
		t.Fatalf("ordinary value = %v, want it readable", config["user"])
	}
	original, _, _ := unstructured.NestedMap(object, "spec", "config")
	if original["password"] != "hunter2" {
		t.Fatalf("masking changed the observed object: %v", original["password"])
	}
}

// sensitiveDefinition is a valid application definition with one sensitive
// config field (C-31, C-32).
func sensitiveDefinition() *unstructured.Unstructured {
	definition := &v1beta1.ApplicationDefinition{
		TypeMeta:   metav1.TypeMeta{APIVersion: api.Group + "/v1beta1", Kind: "ApplicationDefinition"},
		ObjectMeta: metav1.ObjectMeta{Name: "db"},
		Spec: v1beta1.ApplicationDefinitionSpec{
			Package: v1beta1.PackageIdentity{Version: "1.0.0"},
			Config: []v1beta1.ConfigField{
				{Name: "password", Type: "string", Sensitive: true},
				{Name: "user", Type: "string"},
			},
			Template: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Name }}\n",
		},
	}
	digest, err := app.DefinitionDigest(definition.Spec)
	if err != nil {
		panic(err)
	}
	definition.Spec.Package.Digest = digest
	document, err := runtime.DefaultUnstructuredConverter.ToUnstructured(definition)
	if err != nil {
		panic(err)
	}
	return &unstructured.Unstructured{Object: document}
}

// storedApplication is one stored application carrying a sensitive value.
func storedApplication() map[string]any {
	return map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "one", "namespace": "default", "resourceVersion": "7"},
		"spec": map[string]any{
			"definitionRef": "db",
			"namespace":     "apps",
			"config":        map[string]any{"password": "hunter2", "user": "app"},
		},
	}
}

// sensitiveServer serves one stored application and its definition.
func sensitiveServer() *Server {
	return &Server{
		source:     &fakeSource{objects: []ObservedObject{observe(storedApplication())}},
		dynamic:    &writeRecorder{current: sensitiveDefinition()},
		authorizer: AllowAll{},
		now:        time.Now,
		log:        slog.New(slog.DiscardHandler),
	}
}

// TestPlanMasksSensitiveConfigDiff covers C-31 on the plan/apply path: a stored
// sensitive value never comes back through Plan.Update[].Changes.
func TestPlanMasksSensitiveConfigDiff(t *testing.T) {
	server := sensitiveServer()
	plan, _, _, failure := server.buildPlan(context.Background(), context.Background(), VerbPlan, SetRequest{
		Objects: []map[string]any{{
			"apiVersion": api.Group + "/v1beta1",
			"kind":       "Application",
			"metadata":   map[string]any{"name": "one", "namespace": "default"},
			"spec": map[string]any{
				"definitionRef": "db",
				"namespace":     "apps",
				"config":        map[string]any{"password": "newpass", "user": "app2"},
			},
		}},
	})
	if failure != nil {
		t.Fatalf("plan: %v", failure)
	}
	if len(plan.Update) != 1 {
		t.Fatalf("plan updates = %d, want 1", len(plan.Update))
	}
	changes := make(map[string]Change, len(plan.Update[0].Changes))
	for _, change := range plan.Update[0].Changes {
		changes[change.Path] = change
	}
	password, found := changes["spec.config.password"]
	if !found {
		t.Fatalf("the plan does not report the sensitive field change: %v", plan.Update[0].Changes)
	}
	if password.From != "***" || password.To != "***" {
		t.Fatalf("sensitive diff = %v -> %v, want the mask on both sides", password.From, password.To)
	}
	user, found := changes["spec.config.user"]
	if !found || user.From != "app" || user.To != "app2" {
		t.Fatalf("ordinary diff = %v, want the submitted values", user)
	}
}

// TestConflictErrorMasksSensitiveCurrent covers C-31 on the 409 path: the
// conflict answer carries the stored object, so its sensitive config is masked.
func TestConflictErrorMasksSensitiveCurrent(t *testing.T) {
	server := sensitiveServer()
	objectType, ok := kernelType("Application")
	if !ok {
		t.Fatal("Application is not a kernel kind")
	}
	ref := ObjectRef{Group: api.Group, Kind: "Application", Namespace: "default", Name: "one"}
	failure := server.writeFailure(context.Background(), objectType,
		apierrors.NewConflict(schema.GroupResource{Group: api.Group, Resource: "applications"}, "one", errors.New("stale")), ref)
	if failure == nil || failure.Code != CodeConflict {
		t.Fatalf("failure = %v, want a conflict", failure)
	}
	config, _, _ := unstructured.NestedMap(failure.Current, "spec", "config")
	if config["password"] != "***" {
		t.Fatalf("conflict current.password = %v, want the mask", config["password"])
	}
	if config["user"] != "app" {
		t.Fatalf("conflict current.user = %v, want it readable", config["user"])
	}
}

// requestServer builds the API surface over the stored application and the
// given definition, so a case exercises the exit a client actually hits.
func requestServer(t *testing.T, definition *unstructured.Unstructured) *Server {
	t.Helper()
	server, err := New(Config{
		Dynamic: &writeRecorder{current: definition},
		Source:  &fakeSource{objects: []ObservedObject{observe(storedApplication())}},
	})
	if err != nil {
		t.Fatalf("build control plane: %v", err)
	}
	return server
}

// updateBody is the application write of the mask cases: it changes both config
// values, so the update passes the no-change check.
func updateBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "one", "namespace": "default"},
		"spec": map[string]any{
			"definitionRef": "db",
			"namespace":     "apps",
			"config":        map[string]any{"password": "newpass", "user": "app2"},
		},
	})
	if err != nil {
		t.Fatalf("encode the body: %v", err)
	}
	return body
}

// assertMaskedConflict checks that a 409 carries the stored object with the
// sensitive value replaced and never the stored value itself (C-31).
func assertMaskedConflict(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, http.StatusConflict, recorder.Body)
	}
	envelope := struct {
		Error *Error `json:"error"`
	}{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode the failure: %v", err)
	}
	failure := envelope.Error
	if failure == nil {
		t.Fatalf("the response carries no failure envelope: %s", recorder.Body)
	}
	if failure.Code != CodeConflict {
		t.Fatalf("code = %q, want %q", failure.Code, CodeConflict)
	}
	config, _, _ := unstructured.NestedMap(failure.Current, "spec", "config")
	if config["password"] != "***" {
		t.Fatalf("conflict current.password = %v, want the mask", config["password"])
	}
	if config["user"] != "app" {
		t.Fatalf("conflict current.user = %v, want it readable", config["user"])
	}
	if strings.Contains(recorder.Body.String(), "hunter2") {
		t.Fatalf("the 409 body carries the stored sensitive value: %s", recorder.Body)
	}
}

// TestUpdatePreconditionConflictMasksSensitiveCurrent covers C-31 on the update
// exit: a stale If-Match answers with the stored object, masked.
func TestUpdatePreconditionConflictMasksSensitiveCurrent(t *testing.T) {
	server := requestServer(t, sensitiveDefinition())
	request := httptest.NewRequest(http.MethodPut, "/api/v1/resources/Application/one?namespace=default",
		bytes.NewReader(updateBody(t)))
	request.Header.Set("If-Match", "1")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	assertMaskedConflict(t, recorder)
}

// TestDeletePreconditionConflictMasksSensitiveCurrent covers C-31 on the delete
// exit: a stale If-Match answers with the stored object, masked.
func TestDeletePreconditionConflictMasksSensitiveCurrent(t *testing.T) {
	server := requestServer(t, sensitiveDefinition())
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/resources/Application/one?namespace=default", nil)
	request.Header.Set("If-Match", "1")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	assertMaskedConflict(t, recorder)
}

// TestGetMasksTheWholeConfigWhenTheDefinitionIsGone covers C-31 when the
// definition cannot resolve: the marked keys are unknown, so every config value
// is masked rather than read back (over-masking is the safe side).
func TestGetMasksTheWholeConfigWhenTheDefinitionIsGone(t *testing.T) {
	server := requestServer(t, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/resources/Application/one?namespace=default", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, http.StatusOK, recorder.Body)
	}
	read := map[string]any{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the projection: %v", err)
	}
	for _, key := range []string{"password", "user"} {
		if got := nestedText(read, "object", "spec", "config", key); got != "***" {
			t.Fatalf("config.%s = %q, want the mask while the definition is missing", key, got)
		}
	}
	if strings.Contains(recorder.Body.String(), "hunter2") || strings.Contains(recorder.Body.String(), `"user":"app"`) {
		t.Fatalf("the projection carries a stored config value: %s", recorder.Body)
	}
}

// TestPlanMasksEveryConfigDiffWhenTheDefinitionIsGone covers the plan side of
// the same hole: without a definition every config value in the diff is masked.
func TestPlanMasksEveryConfigDiffWhenTheDefinitionIsGone(t *testing.T) {
	server := planServer(storedApplication())
	server.dynamic = &writeRecorder{}
	plan, _, _, failure := server.buildPlan(context.Background(), context.Background(), VerbPlan, SetRequest{
		Objects: []map[string]any{{
			"apiVersion": api.Group + "/v1beta1",
			"kind":       "Application",
			"metadata":   map[string]any{"name": "one", "namespace": "default"},
			"spec": map[string]any{
				"definitionRef": "db",
				"namespace":     "apps",
				"config":        map[string]any{"password": "newpass", "user": "app2"},
			},
		}},
	})
	if failure != nil {
		t.Fatalf("plan: %v", failure)
	}
	if len(plan.Update) != 1 {
		t.Fatalf("plan updates = %d, want 1", len(plan.Update))
	}
	for _, change := range plan.Update[0].Changes {
		if !strings.HasPrefix(change.Path, "spec.config.") {
			continue
		}
		if change.From != "***" || change.To != "***" {
			t.Fatalf("config diff %q = %v -> %v, want the mask on both sides", change.Path, change.From, change.To)
		}
	}
}

// storedComponent is one stored cluster-scoped object for the attribution cases.
func storedComponent(labels, annotations map[string]any) *unstructured.Unstructured {
	metadata := map[string]any{"name": "alpha", "resourceVersion": "7"}
	if labels != nil {
		metadata["labels"] = labels
	}
	if annotations != nil {
		metadata["annotations"] = annotations
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "SystemComponent",
		"metadata":   metadata,
	}}
}

// putComponent sends a PUT carrying the given body metadata and returns the
// answer with the recorder that saw the write.
func putComponent(t *testing.T, stored *unstructured.Unstructured, labels, annotations map[string]any) (*httptest.ResponseRecorder, *writeRecorder) {
	t.Helper()
	recorder := &writeRecorder{current: stored}
	server, err := New(Config{
		Dynamic: recorder,
		Source:  &fakeSource{objects: []ObservedObject{observe(stored.DeepCopy().Object)}},
	})
	if err != nil {
		t.Fatalf("build control plane: %v", err)
	}
	metadata := map[string]any{"name": "alpha"}
	if labels != nil {
		metadata["labels"] = labels
	}
	if annotations != nil {
		metadata["annotations"] = annotations
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "SystemComponent",
		"metadata":   metadata,
	})
	if err != nil {
		t.Fatalf("encode the body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/resources/SystemComponent/alpha", bytes.NewReader(body))
	request.Header.Set("If-Match", "7")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response, recorder
}

// updateComponent sends a PUT whose body names only a plain label, so every
// marker on the submitted object has to come from the stored one.
func updateComponent(t *testing.T, stored *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	response, recorder := putComponent(t, stored, map[string]any{"example.test/tier": "web"}, nil)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusAccepted, response.Body)
	}
	if recorder.submitted == nil {
		t.Fatal("the update was never submitted")
	}
	if got := recorder.submitted.GetLabels()["example.test/tier"]; got != "web" {
		t.Fatalf("submitted tier = %q, want the body's label", got)
	}
	return recorder.submitted
}

// TestUpdateKeepsStoredAttribution covers C-02 on the update path: every
// attribution marker the stored object carries survives a body that omits it,
// and an unmanaged object is not adopted by a plain write.
func TestUpdateKeepsStoredAttribution(t *testing.T) {
	const adoptedAt = "2026-01-02T03:04:05Z"
	cases := []struct {
		name        string
		labels      map[string]any
		annotations map[string]any
		want        markers.Attribution
	}{
		{name: "a managed object stays managed", labels: map[string]any{markers.ManagedBy: write.ManagedByValue}, want: markers.AttributionManaged},
		{name: "an unmanaged object stays unmanaged", want: markers.AttributionUnmanaged},
		{
			name:        "an adopted object stays adopted",
			labels:      map[string]any{markers.AdoptedBy: "alice", markers.AdoptionReason: "confirmed"},
			annotations: map[string]any{markers.AdoptedAt: adoptedAt},
			want:        markers.AttributionAdopted,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			submitted := updateComponent(t, storedComponent(test.labels, test.annotations))
			attribution, err := markers.AttributionOf(submitted.GetLabels())
			if err != nil {
				t.Fatalf("submitted attribution is unreadable: %v", err)
			}
			if attribution != test.want {
				t.Fatalf("submitted attribution = %q, want %q", attribution, test.want)
			}
			if test.want != markers.AttributionAdopted {
				return
			}
			if got := submitted.GetLabels()[markers.AdoptedBy]; got != "alice" {
				t.Fatalf("submitted adopted-by = %q, want the stored marker", got)
			}
			if got := submitted.GetLabels()[markers.AdoptionReason]; got != "confirmed" {
				t.Fatalf("submitted adoption-reason = %q, want the stored marker", got)
			}
			if got := submitted.GetAnnotations()[markers.AdoptedAt]; got != adoptedAt {
				t.Fatalf("submitted adopted-at = %q, want %q", got, adoptedAt)
			}
			if err := write.Admit(write.Request{
				Subject: write.SubjectBackupReconciler,
				Object:  write.RefOf(submitted),
				Verb:    write.VerbApply,
				Parts:   []write.Part{write.PartStatus},
				Current: submitted,
			}); err != nil {
				t.Fatalf("the reconciler was denied a status write on the adopted object: %v", err)
			}
		})
	}
}

// TestUpdateKeepsTheDeclinedMarker covers C-02 on the update path: declining is
// the user's decision, so a plain edit does not erase it.
func TestUpdateKeepsTheDeclinedMarker(t *testing.T) {
	submitted := updateComponent(t, storedComponent(map[string]any{markers.Declined: "true"}, nil))
	if got := submitted.GetLabels()[markers.Declined]; got != "true" {
		t.Fatalf("submitted declined = %q, want the stored marker", got)
	}
	if got := submitted.GetLabels()[markers.ManagedBy]; got != "" {
		t.Fatalf("submitted managed-by = %q, want none", got)
	}
}

// TestUpdateRefusesAMarkerItCannotKeep covers C-02's edge: a body that names an
// attribution key cannot rewrite the stored record or grant an attribution, and
// a refused write leaves the object alone.
func TestUpdateRefusesAMarkerItCannotKeep(t *testing.T) {
	const adoptedAt = "2026-01-02T03:04:05Z"
	adopted := func() *unstructured.Unstructured {
		return storedComponent(
			map[string]any{markers.AdoptedBy: "alice", markers.AdoptionReason: "confirmed"},
			map[string]any{markers.AdoptedAt: adoptedAt},
		)
	}
	cases := []struct {
		name   string
		stored *unstructured.Unstructured
		labels map[string]any
		want   string
	}{
		{name: "a rewritten adoption record is refused", stored: adopted(), labels: map[string]any{markers.AdoptedBy: "bob"}, want: markers.AdoptedBy},
		{name: "a conflicting attribution is refused", stored: adopted(), labels: map[string]any{markers.ManagedBy: write.ManagedByValue}, want: markers.ManagedBy},
		{name: "an unattributed object cannot be granted an attribution", stored: storedComponent(nil, nil), labels: map[string]any{markers.ManagedBy: write.ManagedByValue}, want: "归属状态"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response, recorder := putComponent(t, test.stored, test.labels, nil)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusBadRequest, response.Body)
			}
			if recorder.submitted != nil {
				t.Fatal("a refused update wrote the object")
			}
			if !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("body %s does not name %q", response.Body, test.want)
			}
		})
	}
}

// TestUpdateAcceptsACompleteRoundTrip covers C-02's other edge: a body carrying
// the stored markers unchanged is accepted and reports no change.
func TestUpdateAcceptsACompleteRoundTrip(t *testing.T) {
	const adoptedAt = "2026-01-02T03:04:05Z"
	stored := storedComponent(
		map[string]any{markers.AdoptedBy: "alice", markers.AdoptionReason: "confirmed"},
		map[string]any{markers.AdoptedAt: adoptedAt},
	)
	response, recorder := putComponent(t, stored,
		map[string]any{markers.AdoptedBy: "alice", markers.AdoptionReason: "confirmed"},
		map[string]any{markers.AdoptedAt: adoptedAt})
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusAccepted, response.Body)
	}
	var answer struct {
		Accepted   bool `json:"accepted"`
		Idempotent bool `json:"idempotent"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer: %v", err)
	}
	if !answer.Accepted || !answer.Idempotent {
		t.Fatalf("answer = %+v, want an idempotent acceptance", answer)
	}
	if recorder.submitted != nil {
		t.Fatal("an unchanged round trip rewrote the object")
	}
}

// TestPlanStampsAttributionOnlyForCreatedObjects covers C-02 on the set path: a
// create gets the creator's marker, a managed object keeps its own, and an
// unmanaged object is not adopted.
func TestPlanStampsAttributionOnlyForCreatedObjects(t *testing.T) {
	server := planServer(
		object("SystemComponent", "", "managed", map[string]any{markers.ManagedBy: write.ManagedByValue}, nil),
		object("SystemComponent", "", "plain", nil, nil),
	)
	plan, desired, _, failure := server.buildPlan(context.Background(), context.Background(), VerbPlan, SetRequest{
		Objects: []map[string]any{
			object("SystemComponent", "", "managed", nil, nil),
			object("SystemComponent", "", "plain", map[string]any{"example.test/tier": "web"}, nil),
			object("SystemComponent", "", "fresh", nil, nil),
		},
	})
	if failure != nil {
		t.Fatalf("build plan: %v", failure)
	}
	if len(plan.Create) != 1 || plan.Create[0].Ref.Name != "fresh" {
		t.Fatalf("create = %v, want fresh", plan.Create)
	}
	if len(plan.Update) != 1 || plan.Update[0].Ref.Name != "plain" {
		t.Fatalf("update = %v, want plain only", plan.Update)
	}
	for _, change := range plan.Update[0].Changes {
		if change.Path == "metadata.labels."+markers.ManagedBy {
			t.Fatalf("the plan reports an attribution change on an unmanaged object: %v", plan.Update[0].Changes)
		}
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Object == nil || plan.Warnings[0].Object.Name != "fresh" {
		t.Fatalf("warnings = %v, want the create warning for fresh only", plan.Warnings)
	}
	attribution := make(map[string]string, len(desired))
	for _, item := range desired {
		attribution[item.Ref.Name] = item.Object.GetLabels()[markers.ManagedBy]
	}
	if attribution["fresh"] != write.ManagedByValue {
		t.Fatalf("created object attribution = %q, want %q", attribution["fresh"], write.ManagedByValue)
	}
	if attribution["managed"] != write.ManagedByValue {
		t.Fatalf("managed object attribution = %q, want the stored marker", attribution["managed"])
	}
	if attribution["plain"] != "" {
		t.Fatalf("unmanaged object attribution = %q, want none", attribution["plain"])
	}
}

// TestPlanKeepsStoredAttribution covers C-02 on the set path: the preview keeps
// every attribution and decline marker the stored object carries, so the plan
// and the apply agree on the submitted object.
func TestPlanKeepsStoredAttribution(t *testing.T) {
	const adoptedAt = "2026-01-02T03:04:05Z"
	server := planServer(
		object("SystemComponent", "", "adopted",
			map[string]any{markers.AdoptedBy: "alice", markers.AdoptionReason: "confirmed"},
			map[string]any{markers.AdoptedAt: adoptedAt}),
		object("SystemComponent", "", "declined", map[string]any{markers.Declined: "true"}, nil),
		object("SystemComponent", "", "roundtrip",
			map[string]any{markers.AdoptedBy: "alice", markers.AdoptionReason: "confirmed"},
			map[string]any{markers.AdoptedAt: adoptedAt}),
	)
	plan, desired, _, failure := server.buildPlan(context.Background(), context.Background(), VerbPlan, SetRequest{
		Objects: []map[string]any{
			object("SystemComponent", "", "adopted", map[string]any{"example.test/tier": "web"}, nil),
			object("SystemComponent", "", "declined", map[string]any{"example.test/tier": "web"}, nil),
			object("SystemComponent", "", "roundtrip",
				map[string]any{markers.AdoptedBy: "alice", markers.AdoptionReason: "confirmed"},
				map[string]any{markers.AdoptedAt: adoptedAt}),
		},
	})
	if failure != nil {
		t.Fatalf("build plan: %v", failure)
	}
	if len(plan.Update) != 2 {
		t.Fatalf("update = %v, want both objects", plan.Update)
	}
	for _, update := range plan.Update {
		if update.Ref.Name == "roundtrip" {
			t.Fatalf("the plan reports a change for an unchanged round trip: %v", update.Changes)
		}
		if len(update.Changes) != 1 || update.Changes[0].Path != "metadata.labels.example.test/tier" {
			t.Fatalf("changes for %s = %v, want only the body's label", update.Ref.Name, update.Changes)
		}
	}
	for _, item := range desired {
		switch item.Ref.Name {
		case "adopted":
			if got := item.Object.GetLabels()[markers.AdoptedBy]; got != "alice" {
				t.Fatalf("planned adopted-by = %q, want the stored marker", got)
			}
			if got := item.Object.GetLabels()[markers.AdoptionReason]; got != "confirmed" {
				t.Fatalf("planned adoption-reason = %q, want the stored marker", got)
			}
			if got := item.Object.GetAnnotations()[markers.AdoptedAt]; got != adoptedAt {
				t.Fatalf("planned adopted-at = %q, want %q", got, adoptedAt)
			}
		case "declined":
			if got := item.Object.GetLabels()[markers.Declined]; got != "true" {
				t.Fatalf("planned declined = %q, want the stored marker", got)
			}
		}
	}

	_, _, _, failure = server.buildPlan(context.Background(), context.Background(), VerbPlan, SetRequest{
		Objects: []map[string]any{
			object("SystemComponent", "", "adopted", map[string]any{markers.AdoptedBy: "bob"}, nil),
		},
	})
	if failure == nil || failure.Code != CodeInvalidField {
		t.Fatalf("failure = %+v, want the plan to refuse a rewritten adoption record", failure)
	}
}

// TestCreateOfAnExistingAttributedObjectStaysIdempotent covers C-12/C-02: a
// repeated create compares against the attribution the stored object already
// carries, so it neither conflicts nor rewrites the marker.
func TestCreateOfAnExistingAttributedObjectStaysIdempotent(t *testing.T) {
	stored := storedComponent(map[string]any{markers.ManagedBy: write.ManagedByValue}, nil)
	recorder := &writeRecorder{current: stored}
	server, err := New(Config{
		Dynamic: recorder,
		Source:  &fakeSource{objects: []ObservedObject{observe(stored.DeepCopy().Object)}},
	})
	if err != nil {
		t.Fatalf("build control plane: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "SystemComponent",
		"metadata":   map[string]any{"name": "alpha"},
	})
	if err != nil {
		t.Fatalf("encode the body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/resources/SystemComponent", bytes.NewReader(body))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusAccepted, response.Body)
	}
	var answer struct {
		Accepted   bool `json:"accepted"`
		Idempotent bool `json:"idempotent"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer: %v", err)
	}
	if !answer.Accepted || !answer.Idempotent {
		t.Fatalf("answer = %+v, want an idempotent acceptance", answer)
	}
	if recorder.submitted != nil {
		t.Fatal("a repeated create wrote the object again")
	}
}
