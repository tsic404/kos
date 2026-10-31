package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedDatabase writes one SQLite database from the given statements.
func seedDatabase(t *testing.T, path string, statements ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
}

// seedFile writes one file with the given content.
func seedFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// seedVolume writes one volume with two files and a database holding two
// tables, so every baseline item has a value to compare.
func seedVolume(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "data")
	seedFile(t, filepath.Join(directory, "notes.txt"), "hello")
	seedFile(t, filepath.Join(directory, "assets", "logo.bin"), "1234567890")
	seedDatabase(t, filepath.Join(directory, "app.db"),
		"CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)",
		"INSERT INTO users (id, name) VALUES (2, 'grace'), (1, 'ada')",
		"CREATE TABLE posts (id INTEGER PRIMARY KEY, body TEXT)",
		"INSERT INTO posts (id, body) VALUES (1, 'first')",
	)
	return directory
}

// TestMeasureCoversEveryRowAndFile covers C-42's four items: every table's row
// count, every file and the total bytes, with a checksum that is stable.
func TestMeasureCoversEveryRowAndFile(t *testing.T) {
	directory := seedVolume(t)
	baseline, err := Measure([]VolumeDir{{Volume: "data", Path: directory}})
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	info, err := os.Stat(filepath.Join(directory, "app.db"))
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if baseline.Files != 3 {
		t.Errorf("files = %d, want 3", baseline.Files)
	}
	if want := int64(len("hello")+len("1234567890")) + info.Size(); baseline.Bytes != want {
		t.Errorf("bytes = %d, want %d", baseline.Bytes, want)
	}
	if len(baseline.Tables) != 2 {
		t.Fatalf("tables = %d, want 2", len(baseline.Tables))
	}
	if baseline.Tables[0].Name != "posts" || baseline.Tables[0].Rows != 1 {
		t.Errorf("first table = %s/%d rows, want posts/1", baseline.Tables[0].Name, baseline.Tables[0].Rows)
	}
	if baseline.Tables[1].Name != "users" || baseline.Tables[1].Rows != 2 {
		t.Errorf("second table = %s/%d rows, want users/2", baseline.Tables[1].Name, baseline.Tables[1].Rows)
	}
	if baseline.Checksum == "" {
		t.Error("checksum is empty")
	}
	if baseline.Engines != "sqlite" {
		t.Errorf("engines = %q, want sqlite", baseline.Engines)
	}
	repeated, err := Measure([]VolumeDir{{Volume: "data", Path: directory}})
	if err != nil {
		t.Fatalf("measure again: %v", err)
	}
	if got := Compare(baseline, repeated); len(got) != 0 {
		t.Fatalf("repeated measure differs: %v", got)
	}
}

// TestMeasureOrdersRowsByPrimaryKey covers C-42's rule: rows are sorted by
// primary key before the digest, so the insertion order does not change it.
func TestMeasureOrdersRowsByPrimaryKey(t *testing.T) {
	ordered := filepath.Join(t.TempDir(), "ordered")
	seedDatabase(t, filepath.Join(ordered, "app.db"),
		"CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)",
		"INSERT INTO users (id, name) VALUES (1, 'ada'), (2, 'grace')",
	)
	reversed := filepath.Join(t.TempDir(), "reversed")
	seedDatabase(t, filepath.Join(reversed, "app.db"),
		"CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)",
		"INSERT INTO users (id, name) VALUES (2, 'grace'), (1, 'ada')",
	)
	first, err := Measure([]VolumeDir{{Volume: "data", Path: ordered}})
	if err != nil {
		t.Fatalf("measure ordered: %v", err)
	}
	second, err := Measure([]VolumeDir{{Volume: "data", Path: reversed}})
	if err != nil {
		t.Fatalf("measure reversed: %v", err)
	}
	if len(first.Tables) != 1 || len(second.Tables) != 1 {
		t.Fatalf("tables = %d/%d, want 1/1", len(first.Tables), len(second.Tables))
	}
	if first.Tables[0].Checksum != second.Tables[0].Checksum {
		t.Fatalf("row checksum depends on insertion order")
	}
}

// TestCaptureRestoreRoundTrip covers the restore path: a captured volume
// restored into a new directory measures equal to the source on all four items.
func TestCaptureRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	directory := seedVolume(t)
	plane := DirDataPlane{Root: root}
	source := Source{Namespace: "apps", Volume: "data"}
	if err := os.MkdirAll(plane.Locate(source), 0o700); err != nil {
		t.Fatalf("create source location: %v", err)
	}
	if err := copyTree(ctx, directory, plane.Locate(source)); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	recorded, err := Measure([]VolumeDir{{Volume: "data", Path: plane.Locate(source)}})
	if err != nil {
		t.Fatalf("measure source: %v", err)
	}
	run := filepath.Join(t.TempDir(), "run")
	captured, err := plane.Capture(ctx, run, []Source{source})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if diffs := Compare(recorded, captured); len(diffs) != 0 {
		t.Fatalf("the copy differs from the source: %v", diffs)
	}
	destination := Source{Namespace: "apps", Volume: "restored"}
	if err := plane.Restore(ctx, run, []Copy{{Recorded: "data", Destination: plane.Locate(destination)}}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored, err := Measure([]VolumeDir{{Volume: "data", Path: plane.Locate(destination)}})
	if err != nil {
		t.Fatalf("measure restored: %v", err)
	}
	if diffs := Compare(captured, restored); len(diffs) != 0 {
		t.Fatalf("the restored data differs: %v", diffs)
	}
}

// TestCompareListsTamperedItems covers C-42's failure output: a file written
// into the restored data and a changed row are both named.
func TestCompareListsTamperedItems(t *testing.T) {
	ctx := context.Background()
	directory := seedVolume(t)
	plane := DirDataPlane{Root: t.TempDir()}
	source := Source{Namespace: "apps", Volume: "data"}
	if err := os.MkdirAll(plane.Locate(source), 0o700); err != nil {
		t.Fatalf("create source location: %v", err)
	}
	if err := copyTree(ctx, directory, plane.Locate(source)); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	run := filepath.Join(t.TempDir(), "run")
	baseline, err := plane.Capture(ctx, run, []Source{source})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	seedFile(t, filepath.Join(run, DataDir, "data", "extra.txt"), "surprise")
	db, err := sql.Open("sqlite", "file:"+filepath.Join(run, DataDir, "data", "app.db"))
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	if _, err := db.Exec("UPDATE users SET name = 'mallory' WHERE id = 1"); err != nil {
		t.Fatalf("tamper a row: %v", err)
	}
	db.Close()
	observed, err := Measure([]VolumeDir{{Volume: "data", Path: filepath.Join(run, DataDir, "data")}})
	if err != nil {
		t.Fatalf("measure the tampered copy: %v", err)
	}
	diffs := Compare(baseline, observed)
	items := make([]string, 0, len(diffs))
	for _, diff := range diffs {
		items = append(items, diff.Item)
	}
	for _, want := range []string{"files", "bytes", "checksum", "table data/app.db.users checksum"} {
		if !containsString(items, want) {
			t.Errorf("differences %v do not name %q", items, want)
		}
	}
	if reason := DescribeDiffs(diffs); !strings.Contains(reason, "files") {
		t.Errorf("reason %q does not name the file count", reason)
	}
}

// containsString reports whether values holds value.
func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
