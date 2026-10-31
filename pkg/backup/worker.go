package backup

import (
	"context"
	"fmt"
	"sync"

	"github.com/tsic404/kos/api/v1beta1"
)

// Kinds of long data actions the backup component executes for a reconciler
// (C-06, C-42). A round hands one off and returns; a later round observes the
// outcome, so no round pays the copy or checksum time.
const (
	// TaskCapture copies the sources into a run and measures the copy.
	TaskCapture = "capture"
	// TaskCopy writes recorded volumes from a run into destinations.
	TaskCopy = "copy"
	// TaskMeasure measures directories for the four baseline items.
	TaskMeasure = "measure"
)

// Task is one long data action. The reconciler only builds it.
type Task struct {
	// Owner is the UID of the object the action belongs to: a worker routes the
	// result only to that object's reconciler (C-06).
	Owner string
	// Sequence is the action's ordinal on its owner; a result pairs only with
	// the action that started it (C-06).
	Sequence int64
	// Scope is the set of volumes the action reads or writes, so two actions
	// over one run never pair (C-06).
	Scope string
	// Kind is the action: capture, copy or measure.
	Kind string
	// Run is the run directory the action reads or writes.
	Run string
	// Sources are the volumes a capture copies.
	Sources []Source
	// Copies are the volumes a copy writes.
	Copies []Copy
	// Directories are the volumes a measure reads.
	Directories []VolumeDir
}

// actionIdentity is the key an action and its result are paired on: the owning
// object, the action's ordinal, the volume set it touches and the action
// itself (C-06).
func actionIdentity(owner string, sequence int64, scope, kind, run string) string {
	return fmt.Sprintf("%s|%d|%s|%s|%s", owner, sequence, scope, kind, run)
}

// identity is the key a result and its action are paired on.
func (t Task) identity() string {
	return actionIdentity(t.Owner, t.Sequence, t.Scope, t.Kind, t.Run)
}

// Result is the outcome of one task: the measured baseline for a capture or a
// measure, and the failure of a task that could not finish.
type Result struct {
	Task     Task
	Baseline v1beta1.BackupBaseline
	Err      error
}

// Worker executes long data actions outside the reconcile round (C-06, C-42)
// and routes every outcome back to the object that started it.
type Worker interface {
	// Submit hands one task off and returns immediately.
	Submit(task Task)
	// Completed returns the outcomes of one owner's actions, leaving every
	// other object's outcomes for its own reconciler.
	Completed(owner string) []Result
}

// LocalWorker runs tasks in their own goroutines.
type LocalWorker struct {
	plane   DataPlane
	mutex   sync.Mutex
	results []Result
	active  sync.WaitGroup
}

// NewLocalWorker builds a worker over one data plane.
func NewLocalWorker(plane DataPlane) *LocalWorker {
	return &LocalWorker{plane: plane}
}

// Submit implements Worker.
func (w *LocalWorker) Submit(task Task) {
	w.active.Add(1)
	go func() {
		defer w.active.Done()
		result := w.execute(task)
		w.mutex.Lock()
		w.results = append(w.results, result)
		w.mutex.Unlock()
	}()
}

// Completed implements Worker: results of other objects stay queued.
func (w *LocalWorker) Completed(owner string) []Result {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	mine := make([]Result, 0, len(w.results))
	others := w.results[:0]
	for _, result := range w.results {
		if result.Task.Owner == owner {
			mine = append(mine, result)
			continue
		}
		others = append(others, result)
	}
	w.results = others
	return mine
}

// Wait blocks until every submitted task finished. The controllers never call
// it; a driver uses it between rounds so the next round observes the outcome
// deterministically.
func (w *LocalWorker) Wait() {
	w.active.Wait()
}

// execute runs one task against the data plane. A task outlives the round that
// submitted it, so it carries its own context rather than the round's deadline.
func (w *LocalWorker) execute(task Task) Result {
	ctx := context.Background()
	switch task.Kind {
	case TaskCapture:
		baseline, err := w.plane.Capture(ctx, task.Run, task.Sources)
		return Result{Task: task, Baseline: baseline, Err: err}
	case TaskCopy:
		return Result{Task: task, Err: w.plane.Restore(ctx, task.Run, task.Copies)}
	case TaskMeasure:
		baseline, err := Measure(task.Directories)
		return Result{Task: task, Baseline: baseline, Err: err}
	default:
		return Result{Task: task, Err: fmt.Errorf("unknown task kind %q", task.Kind)}
	}
}
