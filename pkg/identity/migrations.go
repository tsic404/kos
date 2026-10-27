package identity

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// migrationFiles are the schema scripts. Every change to the schema is a new
// pair of files: an applied schema is never edited in place (backend
// migration rule), and the down script is what Rollback runs.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migration is one versioned schema change.
type migration struct {
	// Version orders the scripts; it is also the applied marker.
	Version int
	// Name is the readable part of the file name.
	Name string
	// Up creates the schema.
	Up string
	// Down reverses it.
	Down string
}

// loadMigrations reads and orders the embedded scripts. A version missing one
// of its two halves is a build mistake, not a runtime condition.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("identity: read migrations: %w", err)
	}
	byVersion := map[int]*migration{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		direction := ""
		switch {
		case strings.HasSuffix(name, ".up.sql"):
			direction = "up"
		case strings.HasSuffix(name, ".down.sql"):
			direction = "down"
		default:
			return nil, fmt.Errorf("identity: migration %q names no direction", name)
		}
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".up.sql"), ".down.sql")
		number, rest, found := strings.Cut(base, "_")
		if !found {
			return nil, fmt.Errorf("identity: migration %q is not versioned", name)
		}
		version, err := strconv.Atoi(number)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("identity: migration %q has no version", name)
		}
		body, err := migrationFiles.ReadFile(path.Join("migrations", name))
		if err != nil {
			return nil, fmt.Errorf("identity: read %s: %w", name, err)
		}
		entryMigration, ok := byVersion[version]
		if !ok {
			entryMigration = &migration{Version: version, Name: rest}
			byVersion[version] = entryMigration
		}
		if direction == "up" {
			entryMigration.Up = string(body)
		} else {
			entryMigration.Down = string(body)
		}
	}
	ordered := make([]migration, 0, len(byVersion))
	for _, entry := range byVersion {
		if entry.Up == "" || entry.Down == "" {
			return nil, fmt.Errorf("identity: migration %d needs both an up and a down script", entry.Version)
		}
		ordered = append(ordered, *entry)
	}
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].Version < ordered[right].Version })
	for index, entry := range ordered {
		if entry.Version != index+1 {
			return nil, fmt.Errorf("identity: migration versions must run 1..n, found %d at position %d", entry.Version, index+1)
		}
	}
	return ordered, nil
}

// latestSchemaVersion is the version Open migrates to.
func latestSchemaVersion() (int, error) {
	migrations, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	if len(migrations) == 0 {
		return 0, errors.New("identity: no migrations are embedded")
	}
	return migrations[len(migrations)-1].Version, nil
}

// migrate applies every pending script in order. Each script and its version
// marker commit together, so an interrupted upgrade leaves no half-applied
// version behind.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("identity: create schema_migrations: %w", err)
	}
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return err
	}
	for _, entry := range migrations {
		if applied[entry.Version] {
			continue
		}
		if err := s.applyMigration(ctx, entry, true); err != nil {
			return err
		}
	}
	return nil
}

// Rollback reverses the newest applied migration and returns its version. It
// is the operator path for an upgrade that must be withdrawn; the control
// plane is expected to be stopped, as with a restore (C-61).
func (s *Store) Rollback(ctx context.Context) (int, error) {
	migrations, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return 0, err
	}
	var newest *migration
	for index := range migrations {
		if applied[migrations[index].Version] {
			newest = &migrations[index]
		}
	}
	if newest == nil {
		return 0, errors.New("identity: no migration is applied")
	}
	if err := s.applyMigration(ctx, *newest, false); err != nil {
		return 0, err
	}
	return newest.Version, nil
}

// applyMigration runs one script and records or forgets its version in the same
// transaction.
func (s *Store) applyMigration(ctx context.Context, entry migration, up bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("identity: begin migration %d: %w", entry.Version, err)
	}
	defer func() { _ = tx.Rollback() }()
	script := entry.Down
	if up {
		script = entry.Up
	}
	for _, statement := range splitStatements(script) {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("identity: migrate %d %q: %w", entry.Version, entry.Name, err)
		}
	}
	if up {
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			entry.Version, toMillis(s.now())); err != nil {
			return fmt.Errorf("identity: record migration %d: %w", entry.Version, err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = ?`, entry.Version); err != nil {
			return fmt.Errorf("identity: forget migration %d: %w", entry.Version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("identity: commit migration %d: %w", entry.Version, err)
	}
	return nil
}

// appliedVersions reads the migration markers.
func (s *Store) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("identity: read schema_migrations: %w", err)
	}
	defer rows.Close()
	applied := map[int]bool{}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("identity: scan schema_migrations: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identity: read schema_migrations: %w", err)
	}
	return applied, nil
}

// splitStatements separates the scripts into single statements. Line comments
// are removed first, so a semicolon inside prose cannot split a statement; the
// scripts never use a double dash inside a string literal.
func splitStatements(script string) []string {
	statements := make([]string, 0, 8)
	var current []string
	inTrigger := false
	flush := func() {
		text := strings.TrimSpace(strings.Join(current, "\n"))
		current = current[:0]
		for _, part := range strings.Split(text, ";") {
			if statement := strings.TrimSpace(part); statement != "" {
				statements = append(statements, statement)
			}
		}
	}
	for _, line := range strings.Split(script, "\n") {
		if index := strings.Index(line, "--"); index >= 0 {
			line = line[:index]
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		current = append(current, line)
		switch {
		case !inTrigger && strings.HasPrefix(strings.ToUpper(trimmed), "CREATE TRIGGER"):
			inTrigger = true
		case inTrigger && isTriggerEnd(trimmed):
			// A trigger body is one statement even though it holds semicolons
			// of its own; only the closing END ends it.
			inTrigger = false
			statement := strings.TrimSpace(strings.TrimSuffix(strings.Join(current, "\n"), ";"))
			current = current[:0]
			if statement != "" {
				statements = append(statements, statement)
			}
		case !inTrigger && strings.HasSuffix(trimmed, ";"):
			flush()
		}
	}
	flush()
	return statements
}

// isTriggerEnd reports whether a line closes a trigger body.
func isTriggerEnd(line string) bool {
	return strings.EqualFold(strings.TrimSpace(strings.TrimSuffix(line, ";")), "END")
}

// SchemaVersion reports the newest applied migration version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("identity: read schema version: %w", err)
	}
	if !version.Valid {
		return 0, nil
	}
	return int(version.Int64), nil
}

// toMillis is the stored form of an instant.
func toMillis(at time.Time) int64 { return at.UnixMilli() }

// fromMillis is the read form of a stored instant.
func fromMillis(millis int64) time.Time { return time.UnixMilli(millis).UTC() }
