package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/pkg/endpoint"
)

// Verb names what a request does, so one decision covers reads, writes and
// previews (C-12, C-22).
type Verb string

const (
	// VerbRead lists or reads objects.
	VerbRead Verb = "read"
	// VerbCreate creates an object.
	VerbCreate Verb = "create"
	// VerbUpdate rewrites an object.
	VerbUpdate Verb = "update"
	// VerbDelete removes an object.
	VerbDelete Verb = "delete"
	// VerbPlan previews a desired object set.
	VerbPlan Verb = "plan"
	// VerbApply installs a desired object set.
	VerbApply Verb = "apply"
	// VerbEventRead judges whether a session may read one object's events
	// (C-64): an application's events follow C-59, while every other object's
	// events reach only the global Owner and the member owning the object.
	VerbEventRead Verb = "event-read"
)

// Request is what the authorization seam judges.
type Request struct {
	// Verb is what the request does.
	Verb Verb
	// Ref identifies the object, or the class for a collection read.
	Ref ObjectRef
	// Object is the desired state a write or a plan submits; nil for a read and
	// for a delete.
	Object map[string]any
	// Current is the stored object when the caller holds it; nil when it does
	// not exist or was not read.
	Current map[string]any
}

// Grant is what a permitted request may do beyond running (C-59, C-60).
type Grant struct {
	// Actor is the local username behind the request; empty when the surface
	// carries no account identity.
	Actor string
	// Owner is the value spec.owner must carry after the write (C-60). Empty
	// leaves the submitted value alone.
	Owner string
	// Extension names the extension declaring the object's class; empty for a
	// kernel object (C-14).
	Extension string
}

// ErrHidden tells the read path that a session may not see an object (C-59):
// a list drops it and a single read answers as if it were absent.
var ErrHidden = errors.New("controlplane: object is hidden from this session")

// Authorizer decides whether a request may run. C-59 owns the rule and this API
// only carries its outcome, so a kernel write and an extension page action get
// the same answer. A refusal still carries the actor when the seam resolved one,
// so the audit stream records who was refused (C-14).
type Authorizer interface {
	Decide(ctx context.Context, request Request) (Grant, error)
}

// AllowAll authorizes every request and names no actor; it is the default for a
// surface without accounts.
type AllowAll struct{}

// Decide implements Authorizer.
func (AllowAll) Decide(context.Context, Request) (Grant, error) { return Grant{}, nil }

// maxBodyBytes bounds one request body so a large manifest cannot exhaust the
// control plane.
const maxBodyBytes = 1 << 20

// defaultOperationTimeout bounds one accepted write. It is not the client's
// timeout: work continues when the client goes away (C-12).
const defaultOperationTimeout = 2 * time.Minute

// readRetryAttempts and readRetryDelay bound the re-read after a write that lost
// a race: a cached source lags behind the write it just observed.
const (
	readRetryAttempts = 5
	readRetryDelay    = 50 * time.Millisecond
)

// DiscoverySources answers which discovery adapters reported one object
// (C-25). An object may have several sources; the projection lists all of
// them.
type DiscoverySources interface {
	Sources(group, kind, namespace, name string) []string
}

// Config wires the API to its environment.
type Config struct {
	// Dynamic writes kernel objects; required.
	Dynamic dynamic.Interface
	// Source reads projections; defaults to a live source over Dynamic.
	Source ObjectSource
	// Extensions resolves the object classes active extension components
	// declare (C-19); nil serves kernel kinds only.
	Extensions ExtensionKinds
	// Controllers projects the status of extension objects whose controller is
	// unavailable (C-20); nil shows every stored status as observed.
	Controllers ControllerAvailability
	// Discoveries lists the discovery adapters that reported an object (C-25);
	// nil leaves every projection without sources.
	Discoveries DiscoverySources
	// Authorizer judges requests; defaults to AllowAll.
	Authorizer Authorizer
	// EndpointFacts answers the two path-prefix declarations C-43 requires
	// before an http.path endpoint may be written. A nil value means neither is
	// readable, and that shape is refused (ADR-008: path prefixes are off by
	// default).
	EndpointFacts endpoint.Facts
	// VolumeExpansion answers C-38's expansion preflight a capacity plan
	// reports; nil leaves such plans without a preflight warning.
	VolumeExpansion VolumeExpansion
	// Diagnostics answers the evidence chain and the diagnostic bundle (C-26);
	// nil leaves both routes unserved.
	Diagnostics Diagnostics
	// Logs reads the log lines of one pod container (C-64); nil leaves the log
	// route unserved.
	Logs LogSource
	// Now supplies the read clock; defaults to time.Now.
	Now func() time.Time
	// OperationTimeout bounds one accepted write; defaults to two minutes.
	OperationTimeout time.Duration
	// Auditor records accepted writes (C-14); nil records nothing.
	Auditor Auditor
	// Actor names the principal behind one request (C-14); nil or an empty
	// name records the process subject.
	Actor Actor
	// Log records the detail a user-facing message must not carry; defaults to
	// discarding it.
	Log *slog.Logger
}

// Server is the control plane API. It holds the cluster client and never hands
// one to a caller (C-13, F9).
type Server struct {
	dynamic         dynamic.Interface
	source          ObjectSource
	extensions      ExtensionKinds
	controllers     ControllerAvailability
	discoveries     DiscoverySources
	authorizer      Authorizer
	auditor         Auditor
	actor           Actor
	endpointFacts   endpoint.Facts
	volumeExpansion VolumeExpansion
	diagnostics     Diagnostics
	logs            LogSource
	now             func() time.Time
	timeout         time.Duration
	log             *slog.Logger
	operations      context.Context
	stop            context.CancelFunc
	mux             *http.ServeMux
}

// New builds the API surface.
func New(config Config) (*Server, error) {
	if config.Dynamic == nil {
		return nil, errors.New("controlplane: a dynamic client is required")
	}
	server := &Server{
		dynamic:         config.Dynamic,
		source:          config.Source,
		extensions:      config.Extensions,
		controllers:     config.Controllers,
		discoveries:     config.Discoveries,
		authorizer:      config.Authorizer,
		auditor:         config.Auditor,
		actor:           config.Actor,
		endpointFacts:   config.EndpointFacts,
		volumeExpansion: config.VolumeExpansion,
		diagnostics:     config.Diagnostics,
		logs:            config.Logs,
		now:             config.Now,
		timeout:         config.OperationTimeout,
		log:             config.Log,
	}
	if server.source == nil {
		server.source = NewClusterSource(config.Dynamic)
	}
	if server.authorizer == nil {
		server.authorizer = AllowAll{}
	}
	if server.now == nil {
		server.now = time.Now
	}
	if server.timeout <= 0 {
		server.timeout = defaultOperationTimeout
	}
	if server.log == nil {
		server.log = slog.New(slog.DiscardHandler)
	}
	server.operations, server.stop = context.WithCancel(context.Background())
	server.mux = http.NewServeMux()
	server.routes()
	return server, nil
}

// routes registers the API: objects as resources, then the two request kinds
// that take a desired set (C-12, C-62).
func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/v1/resources/{kind}", s.handleList)
	s.mux.HandleFunc("POST /api/v1/resources/{kind}", s.handleCreate)
	s.mux.HandleFunc("GET /api/v1/resources/{kind}/{name}", s.handleGet)
	s.mux.HandleFunc("PUT /api/v1/resources/{kind}/{name}", s.handleUpdate)
	s.mux.HandleFunc("DELETE /api/v1/resources/{kind}/{name}", s.handleDelete)
	s.mux.HandleFunc("POST /api/v1/plan-requests", s.handlePlan)
	s.mux.HandleFunc("POST /api/v1/apply-requests", s.handleApply)
	s.mux.HandleFunc("POST /api/v1/action-requests", s.handleAction)
	s.mux.HandleFunc("GET /api/v1/diagnostics/{namespace}/{name}", s.handleDiagnosticsChain)
	s.mux.HandleFunc("GET /api/v1/diagnostics/{namespace}/{name}/bundle", s.handleDiagnosticsBundle)
	s.mux.HandleFunc("POST /api/v1/diagnostics/bundles", s.handleDiagnosticsExport)
	s.mux.HandleFunc("GET /api/v1/logs/{namespace}/{name}", s.handleApplicationLogs)
	s.mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, notFound("没有这个接口"))
	})
}

// Handler serves the API.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// Close stops accepted writes that have not finished.
func (s *Server) Close() {
	s.stop()
}

// handleList returns one page of projections with their observation time (C-13).
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	kind := r.PathValue("kind")
	objectType, failure := resolveType(kind, query.Get("apiVersion"), s.extensions)
	if failure != nil {
		writeError(w, withIdentity(failure, query.Get("namespace"), ""))
		return
	}
	namespace := query.Get("namespace")
	if !objectType.Namespaced {
		if namespace != "" {
			writeError(w, invalidField("集群作用域对象不能带 namespace"))
			return
		}
	}
	ref := ObjectRef{Group: objectType.Group, Kind: objectType.Kind, Namespace: namespace}
	selector, failure := parseSelector(query.Get("labelSelector"))
	if failure != nil {
		writeError(w, failure)
		return
	}
	// targetApp is the reverse lookup of C-45: the endpoints that expose one
	// application. It is the same predicate the cascade deletes by, so the list
	// and the deletion touch one set.
	targetApp := query.Get("targetApp")
	if targetApp != "" && kind != "Endpoint" {
		writeError(w, invalidField("targetApp 过滤只适用于 Endpoint"))
		return
	}
	size, failure := pageSize(query.Get("limit"))
	if failure != nil {
		writeError(w, failure)
		return
	}
	last, failure := decodePageToken(query.Get("continue"), objectType, namespace, selector.String(), targetApp)
	if failure != nil {
		writeError(w, failure)
		return
	}
	if _, failure := s.authorize(r.Context(), VerbRead, ref, nil, nil); failure != nil {
		writeError(w, failure)
		return
	}

	observed, err := s.source.List(r.Context(), objectType, namespace)
	if err != nil {
		s.log.Error("list failed", "kind", kind, "namespace", namespace, "error", err)
		writeError(w, internal(StageRead, "读取 "+kind+" 失败").about(ref))
		return
	}
	now := s.now()
	projections := make([]Projection, 0, len(observed))
	for _, item := range observed {
		metadata, _ := item.Object["metadata"].(map[string]any)
		if !selector.Matches(labels.Set(stringMap(metadata["labels"]))) {
			continue
		}
		if targetApp != "" && !endpoint.TargetsApp(&unstructured.Unstructured{Object: item.Object}, targetApp) {
			continue
		}
		object := s.projectedObject(objectType, item.Object)
		masked, failure := s.maskSensitive(r.Context(), objectType.Kind, object)
		if failure != nil {
			writeError(w, failure)
			return
		}
		visible, failure := s.visible(r.Context(), refOf(masked, objectType), masked)
		if failure != nil {
			writeError(w, failure)
			return
		}
		if !visible {
			continue
		}
		projection := newProjection(refOf(masked, objectType), masked, item.ObservedAt, now)
		projection.Sources = s.discoverySources(projection.Ref)
		projections = append(projections, projection)
	}
	sortProjections(projections)

	start, ok := pagePosition(projections, last)
	if !ok {
		writeError(w, invalidField("continue 不在本次查询的范围内，请从头读取"))
		return
	}
	end := min(start+size, len(projections))
	page := projections[start:end]
	if failure := s.attachVolumeBindings(r.Context(), page); failure != nil {
		writeError(w, failure)
		return
	}
	response := listResponse{Items: page}
	if end < len(projections) {
		response.Continue = encodePageToken(objectType, namespace, selector.String(), targetApp, page[len(page)-1].Ref.String())
	}
	writeJSON(w, http.StatusOK, response)
}

// handleGet returns one projection (C-13).
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	name := r.PathValue("name")
	objectType, failure := resolveType(kind, r.URL.Query().Get("apiVersion"), s.extensions)
	if failure != nil {
		writeError(w, withIdentity(failure, r.URL.Query().Get("namespace"), name))
		return
	}
	namespace, failure := readNamespace(r, objectType)
	if failure != nil {
		writeError(w, failure)
		return
	}
	ref := ObjectRef{Group: objectType.Group, Kind: objectType.Kind, Namespace: namespace, Name: name}
	observed, err := s.source.Get(r.Context(), objectType, namespace, name)
	if err != nil {
		writeError(w, s.readFailure(err, ref))
		return
	}
	object := s.projectedObject(objectType, observed.Object)
	masked, failure := s.maskSensitive(r.Context(), objectType.Kind, object)
	if failure != nil {
		writeError(w, failure)
		return
	}
	// The stored object is what C-59's visibility predicate reads, so the
	// decision follows the read.
	if _, failure := s.authorize(r.Context(), VerbRead, ref, masked, nil); failure != nil {
		writeError(w, failure)
		return
	}
	page := []Projection{newProjection(refOf(masked, objectType), masked, observed.ObservedAt, s.now())}
	if failure := s.attachVolumeBindings(r.Context(), page); failure != nil {
		writeError(w, failure)
		return
	}
	page[0].Sources = s.discoverySources(page[0].Ref)
	writeJSON(w, http.StatusOK, page[0])
}

// handleCreate writes one object idempotently: the same request submitted twice
// produces the same object and the same answer (C-12).
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	raw, failure := decodeObjectBody(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	objectType, failure := resolveType(kind, text(raw["apiVersion"]), s.extensions)
	if failure != nil {
		writeError(w, withIdentity(failure, firstNonEmpty(nestedText(raw, "metadata", "namespace"), r.URL.Query().Get("namespace")),
			nestedText(raw, "metadata", "name")))
		return
	}
	object, failure := decodeObject(objectType, r.URL.Query().Get("namespace"), raw)
	if failure != nil {
		writeError(w, failure)
		return
	}
	if failure := s.validateDesired(r.Context(), object); failure != nil {
		writeError(w, failure)
		return
	}
	grant, failure := s.authorize(r.Context(), VerbCreate, object.Ref, object.Object.Object, nil)
	if failure != nil {
		writeError(w, failure)
		return
	}
	actor := s.actorOfGrant(r.Context(), grant)
	// The API is the writer of spec.owner: the seam returns the value the
	// session may create under (C-60), and a submitted value never stands.
	if failure := applyOwner(object.Object, grant); failure != nil {
		writeError(w, failure)
		return
	}
	ctx, cancel := context.WithTimeout(s.operations, s.timeout)
	defer cancel()

	if failure := s.validateApplicationWrite(ctx, object.Object); failure != nil {
		writeError(w, s.auditFailure(ctx, actor, grant.Extension, object.Ref, "create", failure))
		return
	}
	if failure := s.materializeApplicationResources(ctx, object.Object); failure != nil {
		writeError(w, s.auditFailure(ctx, actor, grant.Extension, object.Ref, "create", failure))
		return
	}
	current, err := s.source.Get(ctx, object.Type, object.Ref.Namespace, object.Ref.Name)
	switch {
	case err == nil:
		// A repeat of the same create is not a conflict, and this path never
		// rewrites the object, so the comparison keeps the markers the stored
		// object already carries (C-02, C-12).
		existing, failure := withStoredAttribution(object, current.Object)
		if failure != nil {
			writeError(w, s.auditFailure(ctx, actor, grant.Extension, object.Ref, "create", failure))
			return
		}
		if len(changes(current.Object, existing.Object.Object)) > 0 {
			writeError(w, s.auditFailure(ctx, actor, grant.Extension, object.Ref, "create", (&Error{
				Code:               CodeObjectExists,
				Message:            "对象 " + object.Ref.String() + " 已存在且期望状态不同",
				Stage:              StageValidate,
				RequiresUserAction: true,
				SideEffects:        SideEffectsNone,
			}).about(object.Ref)))
			return
		}
		s.audit(ctx, actor, grant.Extension, object.Ref, "create", ResultAccepted)
		writeJSON(w, http.StatusAccepted, writeResponse{Accepted: true, Ref: new(object.Ref), Idempotent: true})
		return
	case !apierrors.IsNotFound(err):
		writeError(w, s.readFailure(err, object.Ref))
		return
	}

	// The control plane is the creator here, so this is the one place the
	// attribution marker is written; an existing object keeps its own (C-02).
	object = withCreationAttribution(object)
	if failure := s.create(ctx, object); failure != nil {
		writeError(w, s.auditFailure(ctx, actor, grant.Extension, object.Ref, "create", failure))
		return
	}
	s.audit(ctx, actor, grant.Extension, object.Ref, "create", ResultAccepted)
	writeJSON(w, http.StatusAccepted, writeResponse{Accepted: true, Ref: new(object.Ref)})
}

// handleUpdate rewrites the client-owned fields of one object; the precondition
// keeps two writers from overwriting each other, and the API server is the one
// that judges it (C-05, C-12).
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	name := r.PathValue("name")
	raw, failure := decodeObjectBody(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	objectType, failure := resolveType(kind, text(raw["apiVersion"]), s.extensions)
	if failure != nil {
		writeError(w, withIdentity(failure, firstNonEmpty(nestedText(raw, "metadata", "namespace"), r.URL.Query().Get("namespace")), name))
		return
	}
	namespace, failure := readNamespace(r, objectType)
	if failure != nil {
		writeError(w, failure)
		return
	}
	object, failure := decodeObject(objectType, namespace, raw)
	if failure != nil {
		writeError(w, failure)
		return
	}
	if object.Ref.Name != name {
		writeError(w, invalidField("请求体的 metadata.name 与请求路径不一致").about(object.Ref))
		return
	}
	if failure := s.validateDesired(r.Context(), object); failure != nil {
		writeError(w, failure)
		return
	}
	ctx, cancel := context.WithTimeout(s.operations, s.timeout)
	defer cancel()

	current, err := s.source.Get(ctx, object.Type, namespace, name)
	if err != nil {
		writeError(w, s.readFailure(err, object.Ref))
		return
	}
	if failure := s.mergeStoredApplicationResources(ctx, object.Object.Object, current.Object); failure != nil {
		writeError(w, failure)
		return
	}
	// Ownership is a fact of the stored object, so the decision follows the
	// read (C-59, C-60).
	grant, failure := s.authorize(r.Context(), VerbUpdate, object.Ref, object.Object.Object, current.Object)
	if failure != nil {
		writeError(w, failure)
		return
	}
	actor := s.actorOfGrant(r.Context(), grant)
	if failure := applyOwner(object.Object, grant); failure != nil {
		writeError(w, s.auditFailure(ctx, actor, grant.Extension, object.Ref, "update", failure))
		return
	}
	if failure := s.validateApplicationWrite(ctx, object.Object); failure != nil {
		writeError(w, s.auditFailure(ctx, actor, grant.Extension, object.Ref, "update", failure))
		return
	}
	if failure := validateEndpointUpdate(current.Object, object); failure != nil {
		writeError(w, s.auditFailure(ctx, actor, grant.Extension, object.Ref, "update", failure))
		return
	}
	// An update never changes attribution: the markers the creator or the user
	// wrote stay, and an unmanaged object does not gain one (C-02).
	object, failure = withStoredAttribution(object, current.Object)
	if failure != nil {
		writeError(w, s.auditFailure(ctx, actor, grant.Extension, object.Ref, "update", failure))
		return
	}
	if len(changes(current.Object, object.Object.Object)) == 0 {
		s.audit(ctx, actor, grant.Extension, object.Ref, "update", ResultAccepted)
		writeJSON(w, http.StatusAccepted, writeResponse{Accepted: true, Ref: new(object.Ref), Idempotent: true})
		return
	}
	precondition := r.Header.Get("If-Match")
	if precondition == "" {
		precondition = nestedText(raw, "metadata", "resourceVersion")
	}
	if precondition == "" {
		writeError(w, s.auditFailure(ctx, actor, grant.Extension, object.Ref, "update",
			invalidField("更新必须带 metadata.resourceVersion 或 If-Match").about(object.Ref)))
		return
	}
	if precondition != nestedText(current.Object, "metadata", "resourceVersion") {
		writeError(w, s.auditFailure(ctx, actor, grant.Extension, object.Ref, "update",
			s.conflictWith(ctx, object.Type, object.Ref,
				"对象 "+object.Ref.String()+" 已被其他写入修改，请重新读取后再提交", current.Object)))
		return
	}

	if failure := s.update(ctx, object, precondition); failure != nil {
		writeError(w, s.auditFailure(ctx, actor, grant.Extension, object.Ref, "update", failure))
		return
	}
	s.audit(ctx, actor, grant.Extension, object.Ref, "update", ResultAccepted)
	writeJSON(w, http.StatusAccepted, writeResponse{Accepted: true, Ref: new(object.Ref)})
}

// handleDelete removes one object behind a version precondition.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	name := r.PathValue("name")
	objectType, failure := resolveType(kind, r.URL.Query().Get("apiVersion"), s.extensions)
	if failure != nil {
		writeError(w, withIdentity(failure, r.URL.Query().Get("namespace"), name))
		return
	}
	namespace, failure := readNamespace(r, objectType)
	if failure != nil {
		writeError(w, failure)
		return
	}
	ref := ObjectRef{Group: objectType.Group, Kind: objectType.Kind, Namespace: namespace, Name: name}
	ctx, cancel := context.WithTimeout(s.operations, s.timeout)
	defer cancel()
	current, err := s.source.Get(ctx, objectType, namespace, name)
	var stored map[string]any
	if err == nil {
		stored = current.Object
	} else if !apierrors.IsNotFound(err) {
		writeError(w, s.readFailure(err, ref))
		return
	}
	// What may be deleted is a fact of the stored object; a name that is
	// already gone is still judged by role so a Guest is refused (C-59, C-60).
	grant, failure := s.authorize(r.Context(), VerbDelete, ref, nil, stored)
	if failure != nil {
		writeError(w, failure)
		return
	}
	actor := s.actorOfGrant(r.Context(), grant)
	if err != nil {
		s.audit(ctx, actor, grant.Extension, ref, "delete", ResultAccepted)
		writeJSON(w, http.StatusAccepted, writeResponse{Accepted: true, Ref: new(ref), Idempotent: true})
		return
	}
	precondition := r.Header.Get("If-Match")
	if precondition == "" {
		writeError(w, s.auditFailure(ctx, actor, grant.Extension, ref, "delete",
			invalidField("删除必须带 If-Match 的 resourceVersion").about(ref)))
		return
	}
	if precondition != nestedText(current.Object, "metadata", "resourceVersion") {
		writeError(w, s.auditFailure(ctx, actor, grant.Extension, ref, "delete",
			s.conflictWith(ctx, objectType, ref,
				"对象 "+ref.String()+" 已被其他写入修改，请重新读取后再删除", current.Object)))
		return
	}
	if failure := s.delete(ctx, objectType, ref, precondition); failure != nil {
		writeError(w, s.auditFailure(ctx, actor, grant.Extension, ref, "delete", failure))
		return
	}
	s.audit(ctx, actor, grant.Extension, ref, "delete", ResultAccepted)
	writeJSON(w, http.StatusAccepted, writeResponse{Accepted: true, Ref: new(ref)})
}

// handlePlan answers the three deviation classes without writing anything
// (C-62). The response is the same plan an apply of the same set produces.
func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	request, failure := decodeSetRequest(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	plan, _, _, failure := s.buildPlan(r.Context(), r.Context(), VerbPlan, request)
	if failure != nil {
		writeError(w, failure)
		return
	}
	writeJSON(w, http.StatusOK, planResponse{Plan: plan})
}

// handleApply accepts a desired set and carries it out. The answer is acceptance
// with the plan's object references; progress is read from the objects
// themselves and a disconnected client cannot stop the work (C-12, C-13).
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	request, failure := decodeSetRequest(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	// The seam reads the session from the request's own context, while the plan's
	// reads run on the operation context so a client that disconnects before the
	// answer is accepted cannot stop the work (C-12, C-59).
	ctx, cancel := context.WithTimeout(s.operations, s.timeout)
	plan, desired, grants, failure := s.buildPlan(ctx, r.Context(), VerbApply, request)
	if failure != nil {
		cancel()
		writeError(w, failure)
		return
	}
	go func() {
		defer cancel()
		s.execute(ctx, plan, desired, grants)
	}()
	writeJSON(w, http.StatusAccepted, writeResponse{Accepted: true, Plan: plan})
}

// create writes one object. A create whose object already holds the same
// desired state is not an error: repeating a write must converge, not fork
// (C-12).
func (s *Server) create(ctx context.Context, object desiredObject) *Error {
	resource := resourceOf(s.dynamic, object.Type, object.Ref.Namespace)
	if _, err := resource.Create(ctx, object.Object, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return s.writeFailure(ctx, object.Type, err, object.Ref)
		}
		current, readErr := s.awaitObject(ctx, object.Type, object.Ref)
		if readErr != nil {
			// The identity already exists, which is what the request asked for; a
			// cached source that has not caught up must not turn a repeat into a
			// failure (C-12).
			s.log.Warn("created object not yet readable; accepting", "object", object.Ref.String(), "error", readErr)
			return nil
		}
		if len(changes(current.Object, object.Object.Object)) == 0 {
			return nil
		}
		return (&Error{
			Code:               CodeObjectExists,
			Message:            "对象 " + object.Ref.String() + " 已存在且期望状态不同",
			Stage:              StageValidate,
			RequiresUserAction: true,
			SideEffects:        SideEffectsNone,
		}).about(object.Ref)
	}
	return nil
}

// update rewrites the client-owned fields of an object. When the caller named a
// precondition, the submitted object carries that version, so a write that
// slipped in between the read and this call is refused by the API server instead
// of being overwritten (C-05).
func (s *Server) update(ctx context.Context, object desiredObject, expected string) *Error {
	resource := resourceOf(s.dynamic, object.Type, object.Ref.Namespace)
	current, err := resource.Get(ctx, object.Ref.Name, metav1.GetOptions{})
	if err != nil {
		return s.writeFailure(ctx, object.Type, err, object.Ref)
	}
	updated := applyDesired(current, object.Object)
	if expected != "" {
		updated.SetResourceVersion(expected)
	}
	if _, err := resource.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return s.writeFailure(ctx, object.Type, err, object.Ref)
	}
	return nil
}

// delete removes one object. The version precondition makes the API server the
// judge: an update that landed since the caller read the object stops the
// delete instead of being destroyed by it (C-05).
func (s *Server) delete(ctx context.Context, object ObjectType, ref ObjectRef, expected string) *Error {
	resource := resourceOf(s.dynamic, object, ref.Namespace)
	current, err := resource.Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return s.writeFailure(ctx, object, err, ref)
	}
	uid := current.GetUID()
	if expected == "" {
		expected = current.GetResourceVersion()
	}
	if err := resource.Delete(ctx, ref.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &expected},
	}); err != nil && !apierrors.IsNotFound(err) {
		return s.writeFailure(ctx, object, err, ref)
	}
	return nil
}

// awaitObject re-reads an object another writer just created: a cached source
// lags behind the write, so the read is retried briefly.
func (s *Server) awaitObject(ctx context.Context, object ObjectType, ref ObjectRef) (ObservedObject, error) {
	var last error
	for attempt := 0; attempt < readRetryAttempts; attempt++ {
		observed, err := s.source.Get(ctx, object, ref.Namespace, ref.Name)
		if err == nil {
			return observed, nil
		}
		last = err
		select {
		case <-ctx.Done():
			return ObservedObject{}, ctx.Err()
		case <-time.After(readRetryDelay):
		}
	}
	return ObservedObject{}, last
}

// decide asks the seam for one request and renders a refusal as the contract's
// stable code (C-12). It records nothing, so a plan can judge the deletions it
// proposes without writing a row (C-62); an object a session may not see is
// answered as absent (C-59).
func (s *Server) decide(ctx context.Context, verb Verb, ref ObjectRef, object, current map[string]any) (Grant, *Error) {
	grant, err := s.authorizer.Decide(ctx, Request{Verb: verb, Ref: ref, Object: object, Current: current})
	if err == nil {
		return grant, nil
	}
	if errors.Is(err, ErrHidden) {
		return grant, notFound("对象 " + ref.String() + " 不存在").about(ref)
	}
	s.log.Info("request refused", "verb", string(verb), "object", ref.String(), "reason", err)
	return grant, (&Error{
		Code:               CodePermissionDenied,
		Message:            "当前身份不能对 " + ref.String() + " 执行 " + string(verb),
		Stage:              StageAuthorize,
		RequiresUserAction: true,
		SideEffects:        SideEffectsNone,
	}).about(ref)
}

// authorize judges one request of a write surface: a refused write leaves a
// record so the attempt is visible (C-14). A read records nothing, and a plan
// records nothing because it changes no row (C-62).
func (s *Server) authorize(ctx context.Context, verb Verb, ref ObjectRef, object, current map[string]any) (Grant, *Error) {
	grant, failure := s.decide(ctx, verb, ref, object, current)
	if failure != nil && changesRows(verb) {
		s.audit(ctx, s.actorOfGrant(ctx, grant), grant.Extension, ref, string(verb), ResultRejected)
	}
	return grant, failure
}

// changesRows reports whether a refused verb leaves an audit record: only a
// verb that would rewrite a row is recorded (C-14). The reads, including the
// event read (C-64), and the plan (C-62) record nothing.
func changesRows(verb Verb) bool {
	switch verb {
	case VerbRead, VerbEventRead, VerbPlan:
		return false
	}
	return true
}

// visible reports whether a session may see one object of a read (C-59). Only
// the hidden answer filters an object out; every other refusal fails the
// request, because it names a session that may not read the class at all.
func (s *Server) visible(ctx context.Context, ref ObjectRef, object map[string]any) (bool, *Error) {
	return s.visibleWithVerb(ctx, VerbRead, ref, object)
}

// visibleWithVerb is visible for one named read verb, so a read that carries
// its own visibility rule (C-64's event rule) is judged by the same seam.
func (s *Server) visibleWithVerb(ctx context.Context, verb Verb, ref ObjectRef, object map[string]any) (bool, *Error) {
	_, err := s.authorizer.Decide(ctx, Request{Verb: verb, Ref: ref, Object: object})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrHidden):
		return false, nil
	default:
		s.log.Info("read refused", "object", ref.String(), "reason", err)
		return false, (&Error{
			Code:               CodePermissionDenied,
			Message:            "当前身份不能读取 " + ref.String(),
			Stage:              StageAuthorize,
			RequiresUserAction: true,
			SideEffects:        SideEffectsNone,
		}).about(ref)
	}
}

// readFailure turns a source error into the failure envelope. The cluster's own
// error text stays in the log: it names endpoints and subjects a browser has no
// business seeing (C-12).
func (s *Server) readFailure(err error, ref ObjectRef) *Error {
	if apierrors.IsNotFound(err) {
		return notFound("对象 " + ref.String() + " 不存在").about(ref)
	}
	s.log.Error("read failed", "object", ref.String(), "error", err)
	return internal(StageRead, "读取对象 "+ref.String()+" 失败").about(ref)
}

// conflictWith renders a conflict answer from the object the cluster holds now.
// Every 409 leaves through here, so a stored sensitive config is masked wherever
// it is read back (C-05, C-31); an object that cannot be masked is dropped
// instead of served.
func (s *Server) conflictWith(ctx context.Context, object ObjectType, ref ObjectRef, message string, current map[string]any) *Error {
	if current != nil {
		masked, failure := s.maskSensitive(ctx, object.Kind, current)
		if failure != nil {
			s.log.Warn("conflict object cannot be masked; dropping it", "object", ref.String(), "error", failure.Message)
			current = nil
		} else {
			current = masked
		}
	}
	return conflict(ref, message, current)
}

// writeFailure turns a cluster write error into the failure envelope. A conflict
// carries the object as the cluster holds it now, so the caller reconciles
// instead of repeating a blind overwrite (C-05).
func (s *Server) writeFailure(ctx context.Context, object ObjectType, err error, ref ObjectRef) *Error {
	switch {
	case apierrors.IsConflict(err):
		s.log.Warn("write conflict", "object", ref.String(), "error", err)
		return s.conflictWith(ctx, object, ref, "对象 "+ref.String()+" 已被其他写入修改，请重新读取后再提交",
			s.currentObject(ctx, object, ref))
	case apierrors.IsInvalid(err):
		s.log.Warn("cluster rejected fields", "object", ref.String(), "error", err)
		return (&Error{
			Code:               CodeInvalidField,
			Message:            "集群拒绝了对象 " + ref.String() + " 的字段",
			Stage:              StageValidate,
			RequiresUserAction: true,
			SideEffects:        SideEffectsNone,
		}).about(ref)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		s.log.Error("control plane may not write", "object", ref.String(), "error", err)
		return (&Error{
			Code:        CodeWriteFailed,
			Message:     "控制面没有写入对象 " + ref.String() + " 的权限，请检查控制面自身的授权",
			Stage:       StageApply,
			Retryable:   false,
			SideEffects: SideEffectsNone,
		}).about(ref)
	default:
		s.log.Error("write failed", "object", ref.String(), "error", err)
		return (&Error{
			Code:        CodeWriteFailed,
			Message:     "写入对象 " + ref.String() + " 失败，请稍后重试",
			Stage:       StageApply,
			Retryable:   true,
			SideEffects: SideEffectsNone,
		}).about(ref)
	}
}

// currentObject reads the object as the cluster holds it now; the conflict
// answer carries it (C-05).
func (s *Server) currentObject(ctx context.Context, object ObjectType, ref ObjectRef) map[string]any {
	observed, err := s.source.Get(ctx, object, ref.Namespace, ref.Name)
	if err != nil {
		s.log.Debug("conflict object unreadable", "object", ref.String(), "error", err)
		return nil
	}
	return observed.Object
}

// writeResponse answers a write with acceptance and the object reference; it
// never claims completion (C-13).
type writeResponse struct {
	// Accepted is always true: a write that failed is an error response.
	Accepted bool `json:"accepted"`
	// Ref identifies the object a single-object write targets; a set apply
	// names its objects through the plan instead.
	Ref *ObjectRef `json:"ref,omitempty"`
	// Idempotent tells that the desired state was already in place.
	Idempotent bool `json:"idempotent,omitempty"`
	// Plan is the diff an apply request accepted (C-62).
	Plan *Plan `json:"plan,omitempty"`
}

// planResponse is the answer to a plan request (C-62).
type planResponse struct {
	Plan *Plan `json:"plan"`
}

// listResponse is one page of projections.
type listResponse struct {
	Items    []Projection `json:"items"`
	Continue string       `json:"continue,omitempty"`
}

// writeJSON writes a JSON body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError writes the failure envelope (C-12).
func writeError(w http.ResponseWriter, failure *Error) {
	writeJSON(w, failure.status(), map[string]any{"error": failure})
}

// WriteError writes the failure envelope for a surface outside this package
// (C-12). The status follows the code, so callers never invent their own.
func WriteError(w http.ResponseWriter, failure *Error) {
	writeError(w, failure)
}

// withIdentity completes a resolution failure with the object the request names,
// so the failure envelope still identifies it (C-12).
func withIdentity(failure *Error, namespace, name string) *Error {
	if failure == nil || failure.Object == nil {
		return failure
	}
	if namespace == "" && name == "" {
		return failure
	}
	ref := *failure.Object
	if ref.Namespace == "" {
		ref.Namespace = namespace
	}
	if ref.Name == "" {
		ref.Name = name
	}
	failure.Object = &ref
	return failure
}

// firstNonEmpty returns the first value that is not empty.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// decodeObjectBody reads one object manifest as generic JSON.
func decodeObjectBody(r *http.Request) (map[string]any, *Error) {
	object := map[string]any{}
	if failure := decodeBody(r, false, &object); failure != nil {
		return nil, failure
	}
	if object == nil {
		return nil, invalid("请求体必须是 JSON 对象")
	}
	return object, nil
}

// decodeSetRequest reads a plan or apply body.
func decodeSetRequest(r *http.Request) (SetRequest, *Error) {
	request := SetRequest{}
	if failure := decodeBody(r, true, &request); failure != nil {
		return SetRequest{}, failure
	}
	return request, nil
}

// decodeBody reads a bounded JSON body, rejecting unknown fields when the target
// is the request envelope rather than an object.
func decodeBody(r *http.Request, strict bool, target any) *Error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return invalid("读取请求体失败")
	}
	if len(body) > maxBodyBytes {
		return invalid("请求体超过 1 MiB")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return invalid("请求体不能为空")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return invalid("请求体不是合法的 JSON 对象：" + err.Error())
	}
	return nil
}

// readNamespace reads the namespace parameter of the resolved class.
func readNamespace(r *http.Request, object ObjectType) (string, *Error) {
	namespace := r.URL.Query().Get("namespace")
	if !object.Namespaced {
		if namespace != "" {
			return "", invalidField("集群作用域对象不能带 namespace")
		}
		return "", nil
	}
	if namespace == "" {
		return "", invalidField("命名空间作用域对象必须给出 namespace 参数")
	}
	return namespace, nil
}

// stringMap converts a raw labels map for selector matching.
func stringMap(value any) map[string]string {
	source, _ := value.(map[string]any)
	converted := make(map[string]string, len(source))
	for key, item := range source {
		if parsed, ok := item.(string); ok {
			converted[key] = parsed
		}
	}
	return converted
}
