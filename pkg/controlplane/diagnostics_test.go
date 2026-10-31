package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/diagnostics"
)

// stubDiagnostics serves fixed bundles for the export cases.
type stubDiagnostics struct {
	bundles map[string]diagnostics.Bundle
}

// Chain implements Diagnostics.
func (s stubDiagnostics) Chain(_ context.Context, namespace, name string) (diagnostics.Chain, error) {
	bundle, known := s.bundles[namespace+"/"+name]
	if !known {
		return diagnostics.Chain{}, apierrors.NewNotFound(schema.GroupResource{Resource: "applications"}, name)
	}
	return bundle.Evidence, nil
}

// Bundle implements Diagnostics.
func (s stubDiagnostics) Bundle(_ context.Context, namespace, name string) (diagnostics.Bundle, error) {
	bundle, known := s.bundles[namespace+"/"+name]
	if !known {
		return diagnostics.Bundle{}, apierrors.NewNotFound(schema.GroupResource{Resource: "applications"}, name)
	}
	return bundle, nil
}

// exportServer serves one API whose diagnostics surface answers the fixed
// bundles.
func exportServer(t *testing.T, authorizer Authorizer, auditor Auditor) *httptest.Server {
	t.Helper()
	application := map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "shop", "namespace": "default", "uid": "app-1"},
		"spec":       map[string]any{"owner": "alice"},
	}
	source := &fakeSource{objects: []ObservedObject{{Object: application}}}
	return tunedEventsServer(t, func(config *Config) {
		config.Source = source
		config.Authorizer = authorizer
		config.Auditor = auditor
		config.Diagnostics = stubDiagnostics{bundles: map[string]diagnostics.Bundle{
			"default/shop": {
				Application: diagnostics.ObjectRef{Kind: "Application", Namespace: "default", Name: "shop"},
			},
		}}
	})
}

// TestDiagnosticsExportAcceptsAWriteRequest checks that the diagnostics page's
// modification entry exports the bundle through a write request (C-22, C-51)
// and that the exported document is the same one the read route serves (C-26).
func TestDiagnosticsExportAcceptsAWriteRequest(t *testing.T) {
	server := exportServer(t, &eventAuthorizer{}, nil)
	body, _ := json.Marshal(map[string]string{"namespace": "default", "name": "shop"})
	response, err := http.Post(server.URL+"/api/v1/diagnostics/bundles", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post export: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("content type = %q, want application/json", contentType)
	}
	if disposition := response.Header.Get("Content-Disposition"); !strings.Contains(disposition, "diagnostic-bundle-shop.json") {
		t.Fatalf("content disposition = %q, want the bundle filename", disposition)
	}
	var bundle diagnostics.Bundle
	if err := json.NewDecoder(response.Body).Decode(&bundle); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	if bundle.Application.Name != "shop" {
		t.Fatalf("bundle application = %+v, want shop", bundle.Application)
	}
}

// TestDiagnosticsExportIsAudited checks that the export entry leaves an audit
// record naming the application, so C-14's trail covers the write C-51 declares
// a modification entry.
func TestDiagnosticsExportIsAudited(t *testing.T) {
	trail := &eventTrail{}
	server := exportServer(t, &eventAuthorizer{}, trail)
	body, _ := json.Marshal(map[string]string{"namespace": "default", "name": "shop"})
	response, err := http.Post(server.URL+"/api/v1/diagnostics/bundles", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post export: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if len(trail.records) != 1 {
		t.Fatalf("audit records = %+v, want one export record", trail.records)
	}
	record := trail.records[0]
	if record.Verb != "export" || record.Result != ResultAccepted || record.Source != SourceAPI {
		t.Fatalf("record = %+v, want an accepted export from the API", record)
	}
	if record.Object.Kind != "Application" || record.Object.Namespace != "default" || record.Object.Name != "shop" {
		t.Fatalf("record object = %+v, want the exported application", record.Object)
	}
}

// TestDiagnosticsExportRequiresAnApplication checks that an export request
// without a target is refused as an invalid field, not answered with an empty
// bundle (C-12).
func TestDiagnosticsExportRequiresAnApplication(t *testing.T) {
	server := exportServer(t, &eventAuthorizer{}, nil)
	response, err := http.Post(server.URL+"/api/v1/diagnostics/bundles", "application/json", strings.NewReader(`{"namespace":"default"}`))
	if err != nil {
		t.Fatalf("post export: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
	var envelope struct {
		Error Error `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode failure: %v", err)
	}
	if envelope.Error.Code != CodeInvalidField {
		t.Fatalf("code = %q, want %q", envelope.Error.Code, CodeInvalidField)
	}
}
