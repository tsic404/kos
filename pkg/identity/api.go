package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/write"
)

// maxBodyBytes bounds one request body, matching the control plane's limit.
const maxBodyBytes = 1 << 20

// Auth paths that stay reachable while a first-login password must change.
const (
	loginPath    = "/api/v1/auth/login"
	logoutPath   = "/api/v1/auth/logout"
	sessionPath  = "/api/v1/auth/session"
	passwordPath = "/api/v1/auth/password"
)

// API serves accounts and sessions and gates the rest of the control plane
// behind a session (C-63, C-68).
type API struct {
	service *Service
}

// NewAPI builds the HTTP surface of the account service.
func NewAPI(service *Service) *API {
	return &API{service: service}
}

// Handler mounts the identity routes and falls through to next for everything
// else. Every route except login requires a session (C-63).
func (a *API) Handler(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+loginPath, a.login)
	mux.HandleFunc("POST "+logoutPath, a.logout)
	mux.HandleFunc("GET "+sessionPath, a.session)
	mux.HandleFunc("PUT "+passwordPath, a.changePassword)
	mux.HandleFunc("GET /api/v1/accounts", a.listAccounts)
	mux.HandleFunc("POST /api/v1/accounts", a.createAccount)
	mux.HandleFunc("PUT /api/v1/accounts/{name}", a.updateAccount)
	mux.HandleFunc("DELETE /api/v1/accounts/{name}", a.deleteAccount)
	mux.Handle("/", next)
	return a.protect(mux)
}

// protect resolves the bearer token, puts the session on the request context
// and refuses requests the session cannot make.
func (a *API) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == loginPath {
			next.ServeHTTP(w, r)
			return
		}
		token, ok := bearerToken(r)
		if !ok {
			controlplane.WriteError(w, failureFor(ErrUnauthenticated))
			return
		}
		session, err := a.service.Authenticate(r.Context(), token)
		if err != nil {
			controlplane.WriteError(w, failureFor(err))
			return
		}
		if session.MustChangePassword && !allowedBeforePasswordChange(r.URL.Path) {
			controlplane.WriteError(w, failureFor(fmt.Errorf("%w: a one-time password is still in use", ErrPasswordChangeRequired)))
			return
		}
		if session.Degraded && isWriteRequest(r) {
			controlplane.WriteError(w, failureFor(ErrUnavailable))
			return
		}
		next.ServeHTTP(w, r.WithContext(withSession(r.Context(), session)))
	})
}

// login trades a login name and password for a session (C-63).
func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if failure := decodeBody(r, &request); failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	login, err := a.service.Login(r.Context(), request.Username, request.Password)
	if err != nil {
		controlplane.WriteError(w, failureFor(err))
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{Token: login.Token, sessionResponse: sessionOf(login.Session)})
}

// logout revokes the session that makes the request.
func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r)
	if !ok {
		controlplane.WriteError(w, failureFor(ErrUnauthenticated))
		return
	}
	if err := a.service.Logout(r.Context(), token); err != nil {
		controlplane.WriteError(w, failureFor(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// session reports the identity and the remaining validity of the session.
func (a *API) session(w http.ResponseWriter, r *http.Request) {
	session, ok := FromContext(r.Context())
	if !ok {
		controlplane.WriteError(w, failureFor(ErrUnauthenticated))
		return
	}
	writeJSON(w, http.StatusOK, sessionOf(session))
}

// changePassword replaces the caller's own password; every other session of
// the account is revoked (C-63).
func (a *API) changePassword(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r)
	if !ok {
		controlplane.WriteError(w, failureFor(ErrUnauthenticated))
		return
	}
	var request struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if failure := decodeBody(r, &request); failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	if err := a.service.ChangePassword(r.Context(), token, request.CurrentPassword, request.NewPassword); err != nil {
		controlplane.WriteError(w, failureFor(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listAccounts returns every account. Only an Owner may read them.
func (a *API) listAccounts(w http.ResponseWriter, r *http.Request) {
	session, ok := FromContext(r.Context())
	if !ok {
		controlplane.WriteError(w, failureFor(ErrUnauthenticated))
		return
	}
	accounts, err := a.service.ListAccounts(r.Context(), session)
	if err != nil {
		controlplane.WriteError(w, failureFor(err))
		return
	}
	items := make([]accountResponse, 0, len(accounts))
	for _, account := range accounts {
		items = append(items, accountResponseOf(account))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// createAccount adds an account with a one-time password (C-63).
func (a *API) createAccount(w http.ResponseWriter, r *http.Request) {
	session, ok := FromContext(r.Context())
	if !ok {
		controlplane.WriteError(w, failureFor(ErrUnauthenticated))
		return
	}
	var request struct {
		Username        string `json:"username"`
		Role            string `json:"role"`
		OneTimePassword string `json:"oneTimePassword"`
	}
	if failure := decodeBody(r, &request); failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	created, err := a.service.CreateAccount(r.Context(), session, NewAccount{
		Username:        request.Username,
		Role:            write.Role(request.Role),
		OneTimePassword: request.OneTimePassword,
	})
	if err != nil {
		controlplane.WriteError(w, failureFor(err))
		return
	}
	writeJSON(w, http.StatusCreated, accountCreatedResponse{
		accountResponse: accountResponseOf(created.Account),
		OneTimePassword: created.OneTimePassword,
	})
}

// updateAccount changes a role or the disabled state (C-63).
func (a *API) updateAccount(w http.ResponseWriter, r *http.Request) {
	session, ok := FromContext(r.Context())
	if !ok {
		controlplane.WriteError(w, failureFor(ErrUnauthenticated))
		return
	}
	var request AccountChange
	if failure := decodeBody(r, &request); failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	account, err := a.service.UpdateAccount(r.Context(), session, r.PathValue("name"), request)
	if err != nil {
		controlplane.WriteError(w, failureFor(err))
		return
	}
	writeJSON(w, http.StatusOK, accountResponseOf(account))
}

// deleteAccount removes an account that no longer owns objects (C-60, C-63).
func (a *API) deleteAccount(w http.ResponseWriter, r *http.Request) {
	session, ok := FromContext(r.Context())
	if !ok {
		controlplane.WriteError(w, failureFor(ErrUnauthenticated))
		return
	}
	if err := a.service.DeleteAccount(r.Context(), session, r.PathValue("name")); err != nil {
		controlplane.WriteError(w, failureFor(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Authorizer is the C-59 gate over control-plane resources: every request must
// carry the session the identity middleware resolved, and a Guest reads only.
type Authorizer struct{}

// Decide implements controlplane.Authorizer.
func (Authorizer) Decide(ctx context.Context, request controlplane.Request) error {
	session, ok := FromContext(ctx)
	if !ok {
		return ErrUnauthenticated
	}
	switch request.Verb {
	case controlplane.VerbRead, controlplane.VerbPlan:
		return nil
	}
	if session.Role == write.RoleGuest {
		return fmt.Errorf("%w: Guest is read-only", ErrPermissionDenied)
	}
	return nil
}

// sessionContextKey is private so no other package can forge a session.
type sessionContextKey struct{}

// withSession records the resolved session on a request context.
func withSession(ctx context.Context, session Session) context.Context {
	return context.WithValue(ctx, sessionContextKey{}, session)
}

// FromContext returns the session the identity middleware resolved.
func FromContext(ctx context.Context) (Session, bool) {
	session, ok := ctx.Value(sessionContextKey{}).(Session)
	return session, ok
}

// sessionResponse is the session as a caller reads it; the remaining bounds are
// how "剩余有效期可读" (C-63) is answered.
type sessionResponse struct {
	// Username is the login name.
	Username string `json:"username"`
	// Role is the current role.
	Role string `json:"role"`
	// MustChangePassword marks a one-time password still in use.
	MustChangePassword bool `json:"mustChangePassword"`
	// ExpiresAt is the earlier of the two deadlines.
	ExpiresAt time.Time `json:"expiresAt"`
	// RemainingSeconds is the usable time left if the session stays idle.
	RemainingSeconds int64 `json:"remainingSeconds"`
	// IdleRemainingSeconds is the idle bound left.
	IdleRemainingSeconds int64 `json:"idleRemainingSeconds"`
	// AbsoluteRemainingSeconds is the fixed bound left.
	AbsoluteRemainingSeconds int64 `json:"absoluteRemainingSeconds"`
}

// loginResponse adds the bearer token to the session.
type loginResponse struct {
	// Token is the bearer token; the caller sends it as "Bearer <token>".
	Token string `json:"token"`
	sessionResponse
}

// accountResponse is one account as the API returns it.
type accountResponse struct {
	// Username is the login name.
	Username string `json:"username"`
	// Role is the current role.
	Role string `json:"role"`
	// Disabled tells whether login is refused.
	Disabled bool `json:"disabled"`
	// MustChangePassword marks a one-time password still in use.
	MustChangePassword bool `json:"mustChangePassword"`
	// CreatedAt is when the account was created.
	CreatedAt time.Time `json:"createdAt"`
}

// accountCreatedResponse adds the generated one-time password. It appears once
// and is not stored anywhere in clear (C-63).
type accountCreatedResponse struct {
	accountResponse
	// OneTimePassword is set only when the service generated it.
	OneTimePassword string `json:"oneTimePassword,omitempty"`
}

// sessionOf renders a session response.
func sessionOf(session Session) sessionResponse {
	return sessionResponse{
		Username:                 session.Username,
		Role:                     string(session.Role),
		MustChangePassword:       session.MustChangePassword,
		ExpiresAt:                session.ExpiresAt,
		RemainingSeconds:         seconds(session.Remaining),
		IdleRemainingSeconds:     seconds(session.IdleRemaining),
		AbsoluteRemainingSeconds: seconds(session.AbsoluteRemaining),
	}
}

// accountResponseOf renders an account response.
func accountResponseOf(account Account) accountResponse {
	return accountResponse{
		Username:           account.Username,
		Role:               string(account.Role),
		Disabled:           account.Disabled,
		MustChangePassword: account.MustChangePassword,
		CreatedAt:          account.CreatedAt,
	}
}

// seconds rounds a duration up so a just-issued bound does not read one second
// short.
func seconds(remaining time.Duration) int64 {
	if remaining <= 0 {
		return 0
	}
	return int64((remaining + time.Second - 1) / time.Second)
}

// failureFor maps a domain failure to the stable envelope (C-12).
func failureFor(err error) *controlplane.Error {
	var (
		locked     *LockedError
		validation *ValidationError
		hasObjects *HasObjectsError
	)
	switch {
	case errors.As(err, &locked):
		return &controlplane.Error{
			Code:               controlplane.CodeAccountLocked,
			Message:            "登录名已锁定，请稍后重试",
			Stage:              controlplane.StageValidate,
			Retryable:          true,
			RequiresUserAction: false,
			SideEffects:        controlplane.SideEffectsNone,
			RetryAfterSeconds:  seconds(locked.Remaining),
		}
	case errors.As(err, &hasObjects):
		objects := make([]controlplane.ObjectRef, 0, len(hasObjects.Objects))
		for _, object := range hasObjects.Objects {
			objects = append(objects, controlplane.ObjectRef{
				Group: api.Group, Kind: object.Kind, Namespace: object.Namespace, Name: object.Name,
			})
		}
		return &controlplane.Error{
			Code:               controlplane.CodeAccountHasObjects,
			Message:            fmt.Sprintf("账号仍拥有 %d 个对象，请先转移归属再删除", len(objects)),
			Stage:              controlplane.StageValidate,
			RequiresUserAction: true,
			SideEffects:        controlplane.SideEffectsNone,
			OwnedObjects:       objects,
		}
	case errors.As(err, &validation):
		return &controlplane.Error{
			Code:               controlplane.CodeInvalidField,
			Message:            validation.Message,
			Stage:              controlplane.StageValidate,
			RequiresUserAction: true,
			SideEffects:        controlplane.SideEffectsNone,
		}
	case errors.Is(err, ErrPasswordChangeRequired):
		return &controlplane.Error{
			Code:               controlplane.CodePasswordChangeRequired,
			Message:            "首次登录必须先修改口令",
			Stage:              controlplane.StageAuthorize,
			RequiresUserAction: true,
			SideEffects:        controlplane.SideEffectsNone,
		}
	case errors.Is(err, ErrLoginFailed):
		return &controlplane.Error{
			Code:               controlplane.CodeLoginFailed,
			Message:            "登录名或口令不正确",
			Stage:              controlplane.StageValidate,
			RequiresUserAction: true,
			SideEffects:        controlplane.SideEffectsNone,
		}
	case errors.Is(err, ErrUnauthenticated):
		return &controlplane.Error{
			Code:               controlplane.CodeUnauthenticated,
			Message:            "会话无效或已过期，请重新登录",
			Stage:              controlplane.StageAuthorize,
			RequiresUserAction: true,
			SideEffects:        controlplane.SideEffectsNone,
		}
	case errors.Is(err, ErrNotFound):
		return &controlplane.Error{
			Code:               controlplane.CodeNotFound,
			Message:            "账号不存在",
			Stage:              controlplane.StageValidate,
			RequiresUserAction: true,
			SideEffects:        controlplane.SideEffectsNone,
		}
	case errors.Is(err, ErrUsernameTaken):
		return &controlplane.Error{
			Code:               controlplane.CodeAccountExists,
			Message:            "登录名已被占用",
			Stage:              controlplane.StageValidate,
			RequiresUserAction: true,
			SideEffects:        controlplane.SideEffectsNone,
		}
	case errors.Is(err, ErrPermissionDenied):
		return &controlplane.Error{
			Code:               controlplane.CodePermissionDenied,
			Message:            "当前身份没有这个权限",
			Stage:              controlplane.StageAuthorize,
			RequiresUserAction: true,
			SideEffects:        controlplane.SideEffectsNone,
		}
	case errors.Is(err, ErrLastOwner):
		return &controlplane.Error{
			Code:               controlplane.CodeLastOwner,
			Message:            "最后一个 Owner 不能被停用、降级或删除",
			Stage:              controlplane.StageValidate,
			RequiresUserAction: true,
			SideEffects:        controlplane.SideEffectsNone,
		}
	case errors.Is(err, ErrCurrentPassword):
		return &controlplane.Error{
			Code:               controlplane.CodeInvalidField,
			Message:            "当前口令不正确",
			Stage:              controlplane.StageValidate,
			RequiresUserAction: true,
			SideEffects:        controlplane.SideEffectsNone,
		}
	case errors.Is(err, ErrOwnershipUnavailable):
		return &controlplane.Error{
			Code:        controlplane.CodeOwnershipCheckFailed,
			Message:     "暂时无法确认账号拥有的对象，请稍后重试",
			Stage:       controlplane.StageRead,
			Retryable:   true,
			SideEffects: controlplane.SideEffectsNone,
		}
	case errors.Is(err, ErrUnavailable):
		return &controlplane.Error{
			Code:        controlplane.CodeDatabaseUnavailable,
			Message:     "控制面数据库暂不可用，请稍后重试",
			Stage:       controlplane.StageApply,
			Retryable:   true,
			SideEffects: controlplane.SideEffectsNone,
		}
	default:
		return &controlplane.Error{
			Code:        controlplane.CodeInternal,
			Message:     "请求失败，请稍后重试",
			Stage:       controlplane.StageApply,
			Retryable:   true,
			SideEffects: controlplane.SideEffectsNone,
		}
	}
}

// decodeBody reads a bounded, strict JSON body.
func decodeBody(r *http.Request, target any) *controlplane.Error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return &controlplane.Error{
			Code: controlplane.CodeInvalidRequest, Message: "读取请求体失败",
			Stage: controlplane.StageValidate, RequiresUserAction: true, SideEffects: controlplane.SideEffectsNone,
		}
	}
	if len(body) > maxBodyBytes {
		return &controlplane.Error{
			Code: controlplane.CodeInvalidRequest, Message: "请求体超过 1 MiB",
			Stage: controlplane.StageValidate, RequiresUserAction: true, SideEffects: controlplane.SideEffectsNone,
		}
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return &controlplane.Error{
			Code: controlplane.CodeInvalidRequest, Message: "请求体不能为空",
			Stage: controlplane.StageValidate, RequiresUserAction: true, SideEffects: controlplane.SideEffectsNone,
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return &controlplane.Error{
			Code: controlplane.CodeInvalidRequest, Message: "请求体不是合法的 JSON 对象：" + err.Error(),
			Stage: controlplane.StageValidate, RequiresUserAction: true, SideEffects: controlplane.SideEffectsNone,
		}
	}
	return nil
}

// writeJSON writes a JSON body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// bearerToken reads the bearer token of a request.
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

// allowedBeforePasswordChange names the only routes a first-login session may
// reach before it replaces its one-time password (C-63).
func allowedBeforePasswordChange(path string) bool {
	switch path {
	case passwordPath, logoutPath, sessionPath:
		return true
	default:
		return false
	}
}

// isWriteRequest classifies a request for the database-unavailable rule: a
// signed-in session keeps reading, writes wait for the database (C-61). A plan
// request writes nothing, so it counts as a read (C-62).
func isWriteRequest(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return !(r.Method == http.MethodPost && r.URL.Path == "/api/v1/plan-requests")
}
