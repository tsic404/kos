// Package settings projects the setting items active extensions declare (C-23):
// each item reports its current value, the writer the value comes from, the
// modification entry when the user may change it, and the reason when it
// cannot. The kernel hardcodes no item; the items disappear with the component
// that declared them.
package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/write"
)

// maxBodyBytes bounds one request body, matching the control plane's limit.
const maxBodyBytes = 1 << 20

// maxValueLength bounds one stored setting value in bytes; it keeps a single
// request from filling the control-plane database.
const maxValueLength = 8192

// Declarations reports the setting items active components declare (C-23).
type Declarations interface {
	// Settings returns every declared item.
	Settings() []extension.Setting
	// Setting resolves one item; false means no active component declares it.
	Setting(id string) (extension.Setting, bool)
}

// Values stores the current value of setting items (C-23). The declared writer
// is the only subject that writes one; the store holds the value the projection
// reads, whatever the writer.
type Values interface {
	// SettingValues reads the stored value of every item in one scope target.
	SettingValues(ctx context.Context, scope, target string) (map[string]string, error)
	// SetSettingValue stores one value.
	SetSettingValue(ctx context.Context, scope, target, identifier, value string) error
}

// Service is the settings domain over the declarations and the value store.
type Service struct {
	declarations Declarations
	values       Values
}

// New builds the settings domain.
func New(declarations Declarations, values Values) *Service {
	return &Service{declarations: declarations, values: values}
}

// Item is one setting item as the panel reads it.
type Item struct {
	// ID is the setting identity the extension declared.
	ID string `json:"id"`
	// Scope is Cluster, Node or User (C-23).
	Scope string `json:"scope"`
	// Writer is the single subject that writes the item (C-23).
	Writer string `json:"writer"`
	// Source names where the current value comes from; it is the declared
	// writer (C-23).
	Source string `json:"source"`
	// Value is the current value.
	Value string `json:"value"`
	// Editable tells whether the user may change the value.
	Editable bool `json:"editable"`
	// Entry is the modification entry; an item the user cannot modify has none.
	Entry *Entry `json:"entry,omitempty"`
	// Reason explains why an item cannot be modified.
	Reason string `json:"reason,omitempty"`
}

// Entry names the request that modifies one item.
type Entry struct {
	// Method is the HTTP method.
	Method string `json:"method"`
	// Path is the request path, without the node parameter.
	Path string `json:"path"`
}

// Items returns every declared item with its current value for this caller. A
// node-scoped item resolves against the named node; the cluster and the caller's
// own account need no node.
func (s *Service) Items(ctx context.Context, session identity.Session, node string) ([]Item, *controlplane.Error) {
	declared := s.declarations.Settings()
	values := map[string]map[string]string{}
	items := make([]Item, 0, len(declared))
	for _, setting := range declared {
		// A node-scoped item is only readable for a named node; a list without
		// one covers the cluster and the caller's account.
		if setting.Scope == extension.SettingScopeNode && strings.TrimSpace(node) == "" {
			continue
		}
		target, failure := targetOf(setting, session, node)
		if failure != nil {
			return nil, failure
		}
		key := setting.Scope + "\x00" + target
		current, read := values[key]
		if !read {
			stored, err := s.values.SettingValues(ctx, setting.Scope, target)
			if err != nil {
				return nil, unavailable()
			}
			current = stored
			values[key] = current
		}
		items = append(items, itemOf(setting, current[setting.ID]))
	}
	return items, nil
}

// Set stores one value and returns the item as it now reads. Only the user
// writer's editable items accept a write, so an item another subject writes
// stays with that subject (C-23).
func (s *Service) Set(ctx context.Context, session identity.Session, id, node, value string) (Item, *controlplane.Error) {
	setting, ok := s.declarations.Setting(id)
	if !ok {
		return Item{}, &controlplane.Error{
			Code:        controlplane.CodeNotFound,
			Message:     "设置项不存在，或提供它的组件未生效",
			Stage:       controlplane.StageValidate,
			SideEffects: controlplane.SideEffectsNone,
		}
	}
	if !setting.Editable || setting.Writer != extension.SettingWriterUser {
		return Item{}, invalid(reasonOf(setting))
	}
	if session.Role == write.RoleGuest {
		return Item{}, &controlplane.Error{
			Code:               controlplane.CodePermissionDenied,
			Message:            "当前身份只读，不能修改设置",
			Stage:              controlplane.StageAuthorize,
			RequiresUserAction: true,
			SideEffects:        controlplane.SideEffectsNone,
		}
	}
	if len(value) > maxValueLength {
		return Item{}, invalid("设置值超过 " + strconv.Itoa(maxValueLength) + " 字节")
	}
	target, failure := targetOf(setting, session, node)
	if failure != nil {
		return Item{}, failure
	}
	if err := s.values.SetSettingValue(ctx, setting.Scope, target, setting.ID, value); err != nil {
		return Item{}, unavailable()
	}
	return itemOf(setting, value), nil
}

// itemOf renders one declared item with a current value.
func itemOf(setting extension.Setting, value string) Item {
	item := Item{
		ID:       setting.ID,
		Scope:    setting.Scope,
		Writer:   setting.Writer,
		Source:   setting.Writer,
		Value:    value,
		Editable: setting.Editable && setting.Writer == extension.SettingWriterUser,
		Reason:   reasonOf(setting),
	}
	if item.Editable {
		item.Entry = &Entry{Method: http.MethodPut, Path: "/api/v1/settings/" + setting.ID}
	}
	return item
}

// reasonOf explains an item the user cannot modify (C-23). An editable item has
// no reason.
func reasonOf(setting extension.Setting) string {
	if setting.Editable && setting.Writer == extension.SettingWriterUser {
		return ""
	}
	switch setting.Writer {
	case extension.SettingWriterController:
		return "设置项由扩展控制器写入，用户不能修改"
	case extension.SettingWriterKernel:
		return "设置项由内核校验器写入，用户不能修改"
	default:
		return "扩展声明该项不可修改"
	}
}

// targetOf resolves which value a read or write addresses. Node scope needs the
// node the caller named; user scope is always the caller's own account, so one
// account never reads another's value (C-23, C-27).
func targetOf(setting extension.Setting, session identity.Session, node string) (string, *controlplane.Error) {
	switch setting.Scope {
	case extension.SettingScopeCluster:
		return "", nil
	case extension.SettingScopeNode:
		if strings.TrimSpace(node) == "" {
			return "", invalid("节点级设置项需要 node 参数")
		}
		if problems := validation.IsDNS1123Subdomain(node); len(problems) > 0 {
			return "", invalid("节点名不是合法的 DNS 子域：" + problems[0])
		}
		return node, nil
	case extension.SettingScopeUser:
		return session.Username, nil
	default:
		return "", &controlplane.Error{
			Code:        controlplane.CodeInternal,
			Message:     "设置项的作用域无法解析",
			Stage:       controlplane.StageRead,
			SideEffects: controlplane.SideEffectsNone,
		}
	}
}

// invalid rejects a request the caller can correct.
func invalid(message string) *controlplane.Error {
	return &controlplane.Error{
		Code:               controlplane.CodeInvalidField,
		Message:            message,
		Stage:              controlplane.StageValidate,
		RequiresUserAction: true,
		SideEffects:        controlplane.SideEffectsNone,
	}
}

// unavailable is the failure of a value store that cannot answer (C-61).
func unavailable() *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeDatabaseUnavailable,
		Message:     "控制面数据库暂不可用，设置暂不可读写",
		Stage:       controlplane.StageApply,
		Retryable:   true,
		SideEffects: controlplane.SideEffectsNone,
	}
}

// API is the HTTP surface of the settings domain.
type API struct {
	service *Service
}

// NewAPI builds the HTTP surface over the domain.
func NewAPI(service *Service) *API { return &API{service: service} }

// Handler serves the settings routes. It expects the identity middleware in
// front of it: every request carries a resolved session (C-63).
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/settings", a.list)
	mux.HandleFunc("PUT /api/v1/settings/{id}", a.set)
	return mux
}

// list returns every declared item with its current value.
func (a *API) list(w http.ResponseWriter, r *http.Request) {
	session, ok := identity.FromContext(r.Context())
	if !ok {
		controlplane.WriteError(w, unauthenticated())
		return
	}
	items, failure := a.service.Items(r.Context(), session, r.URL.Query().Get("node"))
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// set stores one value and answers with the item as it now reads.
func (a *API) set(w http.ResponseWriter, r *http.Request) {
	session, ok := identity.FromContext(r.Context())
	if !ok {
		controlplane.WriteError(w, unauthenticated())
		return
	}
	var request struct {
		Value string `json:"value"`
	}
	if failure := decodeBody(r, &request); failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	item, failure := a.service.Set(r.Context(), session, r.PathValue("id"), r.URL.Query().Get("node"), request.Value)
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	writeJSON(w, http.StatusOK, item)
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

// decodeBody reads a bounded, strict JSON body.
func decodeBody(r *http.Request, target any) *controlplane.Error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return requestInvalid("读取请求体失败")
	}
	if len(body) > maxBodyBytes {
		return requestInvalid("请求体超过 1 MiB")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return requestInvalid("请求体不能为空")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return requestInvalid("请求体不是合法的 JSON 对象")
	}
	return nil
}

// requestInvalid rejects a body the caller can correct.
func requestInvalid(message string) *controlplane.Error {
	return &controlplane.Error{
		Code:               controlplane.CodeInvalidRequest,
		Message:            message,
		Stage:              controlplane.StageValidate,
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
