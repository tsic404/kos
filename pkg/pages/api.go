package pages

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/identity"
)

// API is the HTTP surface of the page shell.
type API struct {
	service *Service
}

// NewAPI builds the HTTP surface over the shell.
func NewAPI(service *Service) *API { return &API{service: service} }

// Handler serves the page routes. It expects the identity middleware in front
// of it: every request carries a resolved session (C-63).
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pages", a.list)
	mux.HandleFunc("GET /api/v1/pages/{id}", a.read)
	return mux
}

// list returns the pages the caller's role may see (C-22).
func (a *API) list(w http.ResponseWriter, r *http.Request) {
	session, ok := identity.FromContext(r.Context())
	if !ok {
		controlplane.WriteError(w, unauthenticated())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": a.service.List(session)})
}

// read renders one page, trimmed to the caller's role (C-22, C-53). The query
// names the object a page that shows one object is read for (C-52).
func (a *API) read(w http.ResponseWriter, r *http.Request) {
	session, ok := identity.FromContext(r.Context())
	if !ok {
		controlplane.WriteError(w, unauthenticated())
		return
	}
	target, failure := targetFromQuery(r.URL.Query())
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	page, failure := a.service.Read(r.Context(), session, r.PathValue("id"), target)
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// targetFromQuery reads the object a page read is about. No class, namespace or
// name is a panel read; naming part of one is a field failure, because the shell
// cannot tell which object the caller meant (C-52).
func targetFromQuery(query url.Values) (Target, *controlplane.Error) {
	target := Target{Kind: query.Get("kind"), Namespace: query.Get("namespace"), Name: query.Get("name")}
	if !target.Named() || target.Complete() {
		return target, nil
	}
	return Target{}, &controlplane.Error{
		Code:        controlplane.CodeInvalidField,
		Message:     "读取单对象页面必须同时给出 kind 与 name",
		Stage:       controlplane.StageValidate,
		SideEffects: controlplane.SideEffectsNone,
	}
}

// unauthenticated is the answer to a request without a session (C-63).
func unauthenticated() *controlplane.Error {
	return &controlplane.Error{
		Code:               controlplane.CodeUnauthenticated,
		Message:            "会话无效或已过期，请重新登录",
		Stage:              controlplane.StageAuthorize,
		RequiresUserAction: true,
		SideEffects:        controlplane.SideEffectsNone,
	}
}

// writeJSON writes a JSON body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
