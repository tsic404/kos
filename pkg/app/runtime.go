package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/write"
)

// DefaultActionSweep bounds how long the record of an application that is gone
// may stay in the KOS namespace when no interval is set (C-69).
const DefaultActionSweep = 2 * time.Minute

// applicationResource resolves the served application class the watch follows.
var applicationResource = schema.GroupVersionResource{Group: api.Group, Version: "v1beta1", Resource: "applications"}

// RuntimeOptions configures the watch wiring of the application reconciler.
type RuntimeOptions struct {
	// Workers is the number of concurrent rounds; values below one mean one.
	Workers int
	// Sweep is the interval of the action-record sweep; zero selects
	// DefaultActionSweep.
	Sweep time.Duration
}

// Runtime drives one application controller from the object stream: the
// Application informer enqueues created and changed applications, the record
// informer enqueues the application a record names, and a periodic sweep
// deletes the records of applications that are gone (C-33, C-69).
//
// The watch adds rounds; it never replaces the bounded requeue of a round.
// A settled application keeps its own schedule (C-13), and a Failed one
// schedules nothing, so its record is the only way back (C-33).
type Runtime struct {
	controller   *Controller
	rounds       *reconcile.Controller
	applications cache.SharedIndexInformer
	records      cache.SharedIndexInformer
	sweep        time.Duration
}

// NewRuntime wires one application controller to the cluster's object stream.
func NewRuntime(controller *Controller, client dynamic.Interface, options RuntimeOptions) (*Runtime, error) {
	if controller == nil || controller.store == nil {
		return nil, errors.New("app: the runtime needs a controller with a store")
	}
	if client == nil {
		return nil, errors.New("app: the runtime needs a client")
	}
	rounds, err := reconcile.NewController(controller, reconcile.Options{Workers: options.Workers})
	if err != nil {
		return nil, err
	}
	sweep := options.Sweep
	if sweep <= 0 {
		sweep = DefaultActionSweep
	}
	runtime := &Runtime{controller: controller, rounds: rounds, sweep: sweep}
	runtime.applications = dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, 0, metav1.NamespaceAll, nil).
		ForResource(applicationResource).Informer()
	runtime.records = dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, 0, api.SystemNamespace, func(options *metav1.ListOptions) {
		options.LabelSelector = ActionLabel
	}).ForResource(configMapResource).Informer()

	if _, err := runtime.applications.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: runtime.enqueueApplication,
		UpdateFunc: func(previous, current any) {
			if !applicationChanged(previous, current) {
				return
			}
			runtime.enqueueApplication(current)
		},
	}); err != nil {
		return nil, err
	}
	if _, err := runtime.records.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    runtime.enqueueRecord,
		UpdateFunc: func(_, current any) { runtime.enqueueRecord(current) },
	}); err != nil {
		return nil, err
	}
	return runtime, nil
}

// Run starts the watch sources and the round loop and blocks until ctx is done.
// Records left behind by an application that disappeared while no controller
// was running are swept once the caches have synced, so a restart does not wait
// a whole interval before the first cleanup (C-69).
func (r *Runtime) Run(ctx context.Context) {
	stop := ctx.Done()
	go r.applications.Run(stop)
	go r.records.Run(stop)
	if !cache.WaitForCacheSync(stop, r.applications.HasSynced, r.records.HasSynced) {
		return
	}
	r.sweepActionRecords(ctx)

	rounds := make(chan struct{})
	go func() {
		defer close(rounds)
		r.rounds.Run(ctx)
	}()
	ticker := time.NewTicker(r.sweep)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			<-rounds
			return
		case <-ticker.C:
			r.sweepActionRecords(ctx)
		}
	}
}

// enqueueApplication asks for a round of the application an event carries.
func (r *Runtime) enqueueApplication(object any) {
	stored, ok := object.(*unstructured.Unstructured)
	if !ok || stored == nil {
		return
	}
	r.enqueue(stored.GetNamespace(), stored.GetName())
}

// enqueueRecord asks for a round of the application an action record names
// (C-33). A Failed application schedules no further round, so the record is the
// only way the retry it carries reaches the reconciler.
func (r *Runtime) enqueueRecord(object any) {
	stored, ok := object.(*unstructured.Unstructured)
	if !ok || stored == nil {
		return
	}
	target, ok := recordTarget(stored)
	if !ok {
		return
	}
	r.enqueue(target.Namespace, target.Name)
}

// enqueue adds one application request to the round queue. A request already
// waiting collapses into the waiting one, and a delayed requeue of the same
// object stays scheduled: the two sources add rounds instead of replacing each
// other (C-05).
func (r *Runtime) enqueue(namespace, name string) {
	if namespace == "" || name == "" {
		return
	}
	r.rounds.Enqueue(reconcile.Request{Kind: "Application", Namespace: namespace, Name: name})
}

// recordTarget reads the application a one-shot record names (C-69). The record
// carries "namespace/name" in its data; a record without one names no
// application, and no round can consume it.
func recordTarget(record *unstructured.Unstructured) (reconcile.Request, bool) {
	named, _, err := unstructured.NestedString(record.Object, "data", ActionDataKey)
	if err != nil {
		return reconcile.Request{}, false
	}
	namespace, name, ok := strings.Cut(named, "/")
	if !ok || namespace == "" || name == "" {
		return reconcile.Request{}, false
	}
	return reconcile.Request{Kind: "Application", Namespace: namespace, Name: name}, true
}

// applicationChanged reports whether an update carries a fact a round acts on:
// a new desired state or the start of a deletion. A status write asks for no
// round, so a round's own status write never drives the next one (C-06).
func applicationChanged(previous, current any) bool {
	before, okBefore := previous.(*unstructured.Unstructured)
	after, okAfter := current.(*unstructured.Unstructured)
	if !okBefore || !okAfter || before == nil || after == nil {
		return true
	}
	if before.GetGeneration() != after.GetGeneration() {
		return true
	}
	return (before.GetDeletionTimestamp() == nil) != (after.GetDeletionTimestamp() == nil)
}

// sweepActionRecords deletes the one-shot records of applications that are gone
// (C-69). A record is bound to one application by UID and is read only by that
// application's rounds, so a record whose application is absent, or whose name
// now belongs to another UID, has no reader and would stay in the KOS namespace
// for good. The abandonment manifest carries no action label and is never
// swept: it stays after the application object is gone.
func (r *Runtime) sweepActionRecords(ctx context.Context) {
	records, err := r.controller.store.List(ctx, configMapResource, api.SystemNamespace, ActionLabel)
	if err != nil {
		slog.Warn("sweep the action records", "error", err)
		return
	}
	for index := range records {
		record := &records[index]
		orphaned, err := r.recordOrphaned(ctx, record)
		if err != nil {
			slog.Warn("judge an action record", "record", record.GetName(), "error", err)
			continue
		}
		if !orphaned {
			continue
		}
		if err := r.controller.deleteRecord(ctx, record); err != nil {
			slog.Warn("delete an action record", "record", record.GetName(), "error", err)
		}
	}
}

// recordOrphaned reports whether no application can consume a record any more:
// its application is absent, its UID changed, or the record names no target at
// all. A read that failed is not a fact about the application, so the record is
// left to the next sweep.
func (r *Runtime) recordOrphaned(ctx context.Context, record *unstructured.Unstructured) (bool, error) {
	target, ok := recordTarget(record)
	if !ok {
		return true, nil
	}
	stored, err := r.controller.store.Get(ctx, write.Ref{
		Group: api.Group, Version: "v1beta1", Kind: "Application",
		Namespace: target.Namespace, Name: target.Name,
	})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	owner := record.GetLabels()[ActionOwnerLabel]
	return owner == "" || string(stored.GetUID()) != owner, nil
}
