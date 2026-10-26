package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ExportStatus is what the control plane can show about the daily export: the
// skip reason when no target is configured is the observable hint C-61 asks
// for.
type ExportStatus struct {
	// LastAttemptAt is when Export last ran.
	LastAttemptAt time.Time `json:"lastAttemptAt"`
	// LastSuccessAt is when an export last completed.
	LastSuccessAt time.Time `json:"lastSuccessAt"`
	// Path is the newest export file.
	Path string `json:"path,omitempty"`
	// Skipped tells that this attempt wrote nothing.
	Skipped bool `json:"skipped"`
	// Reason explains a skip or a failure.
	Reason string `json:"reason,omitempty"`
}

// ExportResult is one export outcome.
type ExportResult struct {
	// Skipped tells that no file was written.
	Skipped bool
	// Path is the written file.
	Path string
	// ExportedAt is the instant the snapshot represents.
	ExportedAt time.Time
	// Reason explains a skip.
	Reason string
}

// Exporter writes the control-plane database to a configured backup target
// once a day (C-61).
type Exporter struct {
	// Store is the database to export; required.
	Store *Store
	// Target is the directory exports are written to; empty means skip and
	// report why.
	Target string
	// Now supplies the clock; defaults to time.Now.
	Now func() time.Time
	// Log records failures; defaults to discarding them.
	Log *slog.Logger
	// After supplies the wait between daily runs; it defaults to a real timer,
	// and a test replaces it to drive Run with the same clock Export reads.
	After func(ctx context.Context, d time.Duration) <-chan time.Time

	mu     sync.Mutex
	status ExportStatus
}

// Export writes one snapshot. It returns a skip result, not an error, when no
// target is configured: that is a deliberate configuration, and the status
// carries the hint (C-61).
func (e *Exporter) Export(ctx context.Context) (ExportResult, error) {
	now := e.now()
	if e.Store == nil {
		return ExportResult{}, errors.New("identity: an exporter needs a store")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status.LastAttemptAt = now
	if e.Target == "" {
		e.status.Skipped = true
		e.status.Reason = "未配置备份目标，跳过本次数据库导出"
		e.logger().Warn("database export skipped", "reason", "no backup target configured")
		return ExportResult{Skipped: true, Reason: e.status.Reason}, nil
	}
	if err := os.MkdirAll(e.Target, 0o700); err != nil {
		return e.fail(fmt.Errorf("identity: create backup target: %w", err))
	}
	file := filepath.Join(e.Target, "kos-db-"+now.UTC().Format("20060102")+".db")
	temporary := file + ".tmp"
	if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return e.fail(fmt.Errorf("identity: clear partial export: %w", err))
	}
	if err := e.Store.ExportTo(ctx, temporary); err != nil {
		return e.fail(err)
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		return e.fail(fmt.Errorf("identity: restrict export permissions: %w", err))
	}
	if err := os.Rename(temporary, file); err != nil {
		return e.fail(fmt.Errorf("identity: publish export: %w", err))
	}
	e.status = ExportStatus{LastAttemptAt: now, LastSuccessAt: now, Path: file}
	return ExportResult{Path: file, ExportedAt: now}, nil
}

// Status returns the newest attempt.
func (e *Exporter) Status() ExportStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status
}

// Run exports once a day at at, a time of day since midnight. It reads the
// same clock Export does and waits through After, so the loop can be driven by
// a test. It blocks until the context is cancelled; the control plane runs it
// in its own goroutine.
func (e *Exporter) Run(ctx context.Context, at time.Duration) {
	for {
		now := e.now()
		wait := e.after(ctx, nextDailyRun(now, at).Sub(now))
		select {
		case <-ctx.Done():
			return
		case <-wait:
			if _, err := e.Export(ctx); err != nil {
				e.logger().Error("daily database export failed", "error", err)
			}
		}
	}
}

// after waits for d unless ctx ends first. A test-supplied After replaces the
// real timer, so the scheduler needs no sleeping in the test suite.
func (e *Exporter) after(ctx context.Context, d time.Duration) <-chan time.Time {
	if e.After != nil {
		return e.After(ctx, d)
	}
	timer := time.NewTimer(d)
	out := make(chan time.Time, 1)
	go func() {
		defer timer.Stop()
		select {
		case fired := <-timer.C:
			out <- fired
		case <-ctx.Done():
		}
	}()
	return out
}

// nextDailyRun is the first occurrence of at strictly after now.
func nextDailyRun(now time.Time, at time.Duration) time.Time {
	hour := int(at / time.Hour)
	minute := int((at % time.Hour) / time.Minute)
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// fail records a failed attempt.
func (e *Exporter) fail(err error) (ExportResult, error) {
	e.status.Skipped = false
	e.status.Reason = err.Error()
	e.logger().Error("database export failed", "error", err)
	return ExportResult{}, err
}

// now is the exporter clock.
func (e *Exporter) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// logger is the exporter logger; it is never nil.
func (e *Exporter) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Restore replaces the database file with an export. The control plane must be
// stopped: restoring under a running store would leave its handle reading a
// file that is no longer there (C-61).
func Restore(ctx context.Context, exportPath, databasePath string) error {
	if exportPath == "" || databasePath == "" {
		return errors.New("identity: restore needs an export path and a database path")
	}
	source, err := filepath.Abs(exportPath)
	if err != nil {
		return fmt.Errorf("identity: resolve export path: %w", err)
	}
	target, err := filepath.Abs(databasePath)
	if err != nil {
		return fmt.Errorf("identity: resolve database path: %w", err)
	}
	if err := verifyExport(ctx, source); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("identity: create database directory: %w", err)
	}
	temporary := target + ".restore"
	if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("identity: clear partial restore: %w", err)
	}
	if err := copyFile(source, temporary); err != nil {
		return err
	}
	// A stale write-ahead log beside the target would replay onto the restored
	// file and undo the restore.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(target + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("identity: clear stale database sidecar: %w", err)
		}
	}
	if err := os.Rename(temporary, target); err != nil {
		return fmt.Errorf("identity: install restored database: %w", err)
	}
	return nil
}

// verifyExport checks that a file is a complete database this build can open
// and that its schema is not newer than the code.
func verifyExport(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return fmt.Errorf("identity: open export: %w", err)
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("identity: check export: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("identity: export is damaged: %s", integrity)
	}
	var version sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("identity: export has no schema version: %w", err)
	}
	if !version.Valid || version.Int64 < 1 {
		return errors.New("identity: export holds no applied schema")
	}
	latest, err := latestSchemaVersion()
	if err != nil {
		return err
	}
	if version.Int64 > int64(latest) {
		return fmt.Errorf("identity: export schema version %d is newer than this build (%d)", version.Int64, latest)
	}
	return nil
}

// copyFile writes source to target with private permissions and flushes it
// before returning, so a crash cannot leave a half-written database behind the
// rename.
func copyFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("identity: open export: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("identity: create restored database: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("identity: copy export: %w", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("identity: flush restored database: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("identity: close restored database: %w", err)
	}
	return nil
}
