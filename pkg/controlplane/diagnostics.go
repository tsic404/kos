package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/diagnostics"
)

// Diagnostics answers the evidence chain and the diagnostic bundle of one
// application (C-26). The control plane only carries the result; assembling
// belongs to the diagnostics service.
type Diagnostics interface {
	// Chain assembles the evidence chain of one application.
	Chain(ctx context.Context, namespace, name string) (diagnostics.Chain, error)
	// Bundle assembles the diagnostic bundle of one application.
	Bundle(ctx context.Context, namespace, name string) (diagnostics.Bundle, error)
}

// handleDiagnosticsChain returns one application's evidence chain (C-26).
func (s *Server) handleDiagnosticsChain(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	if failure := s.authorizeDiagnostics(r, namespace, name); failure != nil {
		writeError(w, failure)
		return
	}
	chain, err := s.diagnostics.Chain(r.Context(), namespace, name)
	if err != nil {
		writeError(w, s.diagnosticsFailure(err, namespace, name))
		return
	}
	writeJSON(w, http.StatusOK, chain)
}

// handleDiagnosticsBundle returns one application's diagnostic bundle as a
// downloadable document (C-26).
func (s *Server) handleDiagnosticsBundle(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	if failure := s.authorizeDiagnostics(r, namespace, name); failure != nil {
		writeError(w, failure)
		return
	}
	bundle, err := s.diagnostics.Bundle(r.Context(), namespace, name)
	if err != nil {
		writeError(w, s.diagnosticsFailure(err, namespace, name))
		return
	}
	s.writeDiagnosticsBundle(w, bundle, name)
}

// diagnosticsExportRequest names the application one export is about.
type diagnosticsExportRequest struct {
	// Namespace is the application's namespace.
	Namespace string `json:"namespace"`
	// Name is the application name.
	Name string `json:"name"`
}

// handleDiagnosticsExport exports one application's bundle on a write request.
// C-22 admits only a write as a page action and C-51 names the export as the
// diagnostics page's modification entry, so the export is reachable with POST
// as well as the read-only download of C-26. Both answer the same document.
func (s *Server) handleDiagnosticsExport(w http.ResponseWriter, r *http.Request) {
	var request diagnosticsExportRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, invalidField("导出请求必须是 JSON"))
		return
	}
	if request.Namespace == "" || request.Name == "" {
		writeError(w, invalidField("namespace 与 name 必须同时给出"))
		return
	}
	if failure := s.authorizeDiagnostics(r, request.Namespace, request.Name); failure != nil {
		writeError(w, failure)
		return
	}
	bundle, err := s.diagnostics.Bundle(r.Context(), request.Namespace, request.Name)
	if err != nil {
		writeError(w, s.diagnosticsFailure(err, request.Namespace, request.Name))
		return
	}
	// C-14 covers every write attempt, and C-51 declares the export a
	// modification entry, so the accepted export leaves a record naming the
	// application it was about.
	RecordAudit(r.Context(), s.auditor, s.log, AuditRecord{
		Actor:  s.actorOf(r.Context()),
		Object: ObjectRef{Group: api.Group, Kind: "Application", Namespace: request.Namespace, Name: request.Name},
		Verb:   "export",
		Result: ResultAccepted,
		Source: SourceAPI,
	})
	s.writeDiagnosticsBundle(w, bundle, request.Name)
}

// writeDiagnosticsBundle writes the bundle as a downloadable document (C-26).
func (s *Server) writeDiagnosticsBundle(w http.ResponseWriter, bundle diagnostics.Bundle, name string) {
	encoded, err := bundle.Encode()
	if err != nil {
		s.log.Error("encode diagnostic bundle", "application", name, "error", err)
		writeError(w, internal(StageRead, "导出诊断包失败"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "diagnostic-bundle-"+name+".json"))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// authorizeDiagnostics judges the read as the application it is about, so the
// visible-to rule of C-59 applies to diagnostics like any other read.
func (s *Server) authorizeDiagnostics(r *http.Request, namespace, name string) *Error {
	if s.diagnostics == nil {
		return notFound("没有这个接口")
	}
	objectType, failure := resolveType("Application", "", s.extensions)
	if failure != nil {
		return failure
	}
	ref := ObjectRef{Group: api.Group, Kind: "Application", Namespace: namespace, Name: name}
	observed, err := s.source.Get(r.Context(), objectType, namespace, name)
	if err != nil {
		return s.readFailure(err, ref)
	}
	// The stored object is what C-59's visibility predicate reads, so a hidden
	// application is answered as absent here too.
	_, failure = s.authorize(r.Context(), VerbRead, ref, observed.Object, nil)
	return failure
}

// diagnosticsFailure maps an assembly error to the response envelope (C-12).
func (s *Server) diagnosticsFailure(err error, namespace, name string) *Error {
	ref := ObjectRef{Group: api.Group, Kind: "Application", Namespace: namespace, Name: name}
	if apierrors.IsNotFound(err) {
		return notFound("应用不存在").about(ref)
	}
	s.log.Error("diagnostics failed", "application", ref.String(), "error", err)
	return internal(StageRead, "读取诊断失败").about(ref)
}
