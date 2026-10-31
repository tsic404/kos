package backup

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/tsic404/kos/api/v1beta1"
)

// BaselineOrder records the sorting and digest rules every baseline is built
// with (C-42): SQLite tables by database and primary key, files by path, sha256
// over every entry. SQLite is the only table engine this kernel measures; a
// volume without a recognized database records Engines "none" instead of
// passing an empty table list off as measured.
const BaselineOrder = "SQLite tables by database and primary key; files sorted by path; sha256 over every row and file"

// sqliteHeader is the magic every SQLite database file starts with. A file
// without it is measured as content, not opened as a database.
const sqliteHeader = "SQLite format 3\x00"

// VolumeDir names one volume's data directory.
type VolumeDir struct {
	// Volume is the volume object name.
	Volume string
	// Path is the directory holding its data.
	Path string
}

// Measure computes the four baseline items of the named volumes: one entry per
// table, the number of files, the total bytes and a checksum over every row and
// file (C-42). It never samples: every file is read and every row is digested.
func Measure(volumes []VolumeDir) (v1beta1.BackupBaseline, error) {
	baseline := v1beta1.BackupBaseline{Order: BaselineOrder, Engines: "none"}
	var entries []string
	for _, volume := range volumes {
		fileEntries, err := measureFiles(volume, &baseline)
		if err != nil {
			return v1beta1.BackupBaseline{}, err
		}
		entries = append(entries, fileEntries...)
		tableEntries, foundDatabase, err := measureTables(volume, &baseline)
		if err != nil {
			return v1beta1.BackupBaseline{}, err
		}
		if foundDatabase {
			baseline.Engines = "sqlite"
		}
		entries = append(entries, tableEntries...)
	}
	sort.Slice(baseline.Tables, func(left, right int) bool {
		leftTable, rightTable := baseline.Tables[left], baseline.Tables[right]
		if leftTable.Volume != rightTable.Volume {
			return leftTable.Volume < rightTable.Volume
		}
		if leftTable.Database != rightTable.Database {
			return leftTable.Database < rightTable.Database
		}
		return leftTable.Name < rightTable.Name
	})
	sort.Strings(entries)
	digest := sha256.New()
	for _, entry := range entries {
		digest.Write([]byte(entry))
		digest.Write([]byte("\n"))
	}
	baseline.Checksum = hex.EncodeToString(digest.Sum(nil))
	return baseline, nil
}

// measureFiles counts and digests every regular file of one volume (C-42).
func measureFiles(volume VolumeDir, baseline *v1beta1.BackupBaseline) ([]string, error) {
	var entries []string
	err := filepath.WalkDir(volume.Path, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(volume.Path, path)
		if err != nil {
			return err
		}
		sum, err := fileDigest(path)
		if err != nil {
			return err
		}
		baseline.Files++
		baseline.Bytes += info.Size()
		entries = append(entries, fmt.Sprintf("file:%s:%s:%d:%s", volume.Volume, filepath.ToSlash(relative), info.Size(), sum))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("measure volume %s: %w", volume.Volume, err)
	}
	return entries, nil
}

// measureTables digests every table of every database file in one volume; the
// databases are found by content, not by name (C-42). found reports whether the
// volume holds a recognized database at all.
func measureTables(volume VolumeDir, baseline *v1beta1.BackupBaseline) ([]string, bool, error) {
	var entries []string
	found := false
	err := filepath.WalkDir(volume.Path, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		isDatabase, err := isSQLiteFile(path)
		if err != nil {
			return err
		}
		if !isDatabase {
			return nil
		}
		found = true
		relative, err := filepath.Rel(volume.Path, path)
		if err != nil {
			return err
		}
		tableEntries, err := measureDatabase(volume.Volume, filepath.ToSlash(relative), path, baseline)
		if err != nil {
			return err
		}
		entries = append(entries, tableEntries...)
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("measure tables of volume %s: %w", volume.Volume, err)
	}
	return entries, found, nil
}

// measureDatabase digests every table of one database file.
func measureDatabase(volume, database, path string, baseline *v1beta1.BackupBaseline) ([]string, error) {
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tables, err := tableNames(db)
	if err != nil {
		return nil, fmt.Errorf("read tables of %s: %w", database, err)
	}
	var entries []string
	for _, name := range tables {
		table, err := measureTable(db, volume, database, name)
		if err != nil {
			return nil, err
		}
		baseline.Tables = append(baseline.Tables, table)
		entries = append(entries, fmt.Sprintf("rows:%s:%s:%s:%d:%s", table.Volume, table.Database, table.Name, table.Rows, table.Checksum))
	}
	return entries, nil
}

// tableNames lists the user tables of one database, sorted by name.
func tableNames(db *sql.DB) ([]string, error) {
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// tableColumns reads the column names and the primary-key order of one table.
func tableColumns(db *sql.DB, name string) ([]string, []string, error) {
	rows, err := db.Query("PRAGMA table_info(" + quoteIdent(name) + ")")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	type column struct {
		name    string
		primary int
	}
	var columns []column
	for rows.Next() {
		var (
			index      int
			columnName string
			columnType string
			notNull    int
			defaultVal any
			primary    int
		)
		if err := rows.Scan(&index, &columnName, &columnType, &notNull, &defaultVal, &primary); err != nil {
			return nil, nil, err
		}
		columns = append(columns, column{name: columnName, primary: primary})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(columns))
	for _, column := range columns {
		names = append(names, column.name)
	}
	ordered := make([]column, 0, len(columns))
	for _, column := range columns {
		if column.primary > 0 {
			ordered = append(ordered, column)
		}
	}
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].primary < ordered[right].primary })
	key := make([]string, 0, len(ordered))
	for _, column := range ordered {
		key = append(key, column.name)
	}
	return names, key, nil
}

// measureTable counts and digests every row of one table; the rows are read in
// primary-key order, and a table without a primary key is ordered by every
// column so the digest stays deterministic (C-42).
func measureTable(db *sql.DB, volume, database, name string) (v1beta1.TableBaseline, error) {
	columns, key, err := tableColumns(db, name)
	if err != nil {
		return v1beta1.TableBaseline{}, fmt.Errorf("read columns of table %s: %w", name, err)
	}
	if len(columns) == 0 {
		return v1beta1.TableBaseline{}, fmt.Errorf("table %s has no columns", name)
	}
	if len(key) == 0 {
		key = columns
	}
	quoted := make([]string, 0, len(key))
	for _, column := range key {
		quoted = append(quoted, quoteIdent(column))
	}
	query := "SELECT * FROM " + quoteIdent(name) + " ORDER BY " + strings.Join(quoted, ", ")
	rows, err := db.Query(query)
	if err != nil {
		return v1beta1.TableBaseline{}, fmt.Errorf("read rows of table %s: %w", name, err)
	}
	defer rows.Close()
	columnNames, err := rows.Columns()
	if err != nil {
		return v1beta1.TableBaseline{}, err
	}
	digest := sha256.New()
	var count int64
	for rows.Next() {
		values := make([]any, len(columnNames))
		targets := make([]any, len(columnNames))
		for index := range values {
			targets[index] = &values[index]
		}
		if err := rows.Scan(targets...); err != nil {
			return v1beta1.TableBaseline{}, fmt.Errorf("read a row of table %s: %w", name, err)
		}
		digest.Write([]byte(rowLine(values)))
		digest.Write([]byte("\n"))
		count++
	}
	if err := rows.Err(); err != nil {
		return v1beta1.TableBaseline{}, err
	}
	return v1beta1.TableBaseline{
		Volume: volume, Database: database, Name: name,
		Rows: count, Checksum: hex.EncodeToString(digest.Sum(nil)),
	}, nil
}

// rowLine encodes one row so two values of different types never collide.
func rowLine(values []any) string {
	cells := make([]string, 0, len(values))
	for _, value := range values {
		cells = append(cells, cellLine(value))
	}
	return strings.Join(cells, "\x1f")
}

// cellLine encodes one column value with its type.
func cellLine(value any) string {
	switch typed := value.(type) {
	case nil:
		return "n"
	case int64:
		return "i:" + strconv.FormatInt(typed, 10)
	case float64:
		return "f:" + strconv.FormatFloat(typed, 'g', -1, 64)
	case bool:
		return "o:" + strconv.FormatBool(typed)
	case []byte:
		return "b:" + hex.EncodeToString(typed)
	case string:
		return "s:" + typed
	case time.Time:
		return "t:" + typed.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("u:%v", typed)
	}
}

// fileDigest reads one file and returns its sha256.
func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// isSQLiteFile reports whether the file starts with the SQLite magic.
func isSQLiteFile(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	header := make([]byte, len(sqliteHeader))
	read, err := io.ReadFull(file, header)
	if err != nil && err != io.ErrUnexpectedEOF {
		return false, err
	}
	return read == len(sqliteHeader) && string(header) == sqliteHeader, nil
}

// quoteIdent quotes a SQLite identifier.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// readOnlyDSN opens a database file without allowing writes (C-42 measures the
// copy, never the source).
func readOnlyDSN(path string) string {
	base := (&url.URL{Scheme: "file", Path: path}).String()
	return base + "?mode=ro"
}

// Compare returns the differences between a baseline and an observed
// measurement; an empty result means the four items are exactly equal (C-42).
func Compare(baseline, observed v1beta1.BackupBaseline) []v1beta1.BackupDiff {
	var diffs []v1beta1.BackupDiff
	if baseline.Files != observed.Files {
		diffs = append(diffs, v1beta1.BackupDiff{Item: "files", Baseline: strconv.FormatInt(baseline.Files, 10), Observed: strconv.FormatInt(observed.Files, 10)})
	}
	if baseline.Bytes != observed.Bytes {
		diffs = append(diffs, v1beta1.BackupDiff{Item: "bytes", Baseline: strconv.FormatInt(baseline.Bytes, 10), Observed: strconv.FormatInt(observed.Bytes, 10)})
	}
	if baseline.Checksum != observed.Checksum {
		diffs = append(diffs, v1beta1.BackupDiff{Item: "checksum", Baseline: baseline.Checksum, Observed: observed.Checksum})
	}
	recorded := tableMap(baseline.Tables)
	found := tableMap(observed.Tables)
	keys := make([]string, 0, len(recorded))
	for key := range recorded {
		keys = append(keys, key)
	}
	for key := range found {
		if _, exists := recorded[key]; !exists {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		want, recordedHere := recorded[key]
		got, foundHere := found[key]
		switch {
		case !foundHere:
			diffs = append(diffs, v1beta1.BackupDiff{Item: "table " + key + " rows", Baseline: strconv.FormatInt(want.Rows, 10)})
		case !recordedHere:
			diffs = append(diffs, v1beta1.BackupDiff{Item: "table " + key + " rows", Observed: strconv.FormatInt(got.Rows, 10)})
		default:
			if want.Rows != got.Rows {
				diffs = append(diffs, v1beta1.BackupDiff{Item: "table " + key + " rows", Baseline: strconv.FormatInt(want.Rows, 10), Observed: strconv.FormatInt(got.Rows, 10)})
			}
			if want.Checksum != got.Checksum {
				diffs = append(diffs, v1beta1.BackupDiff{Item: "table " + key + " checksum", Baseline: want.Checksum, Observed: got.Checksum})
			}
		}
	}
	return diffs
}

// tableMap indexes the table entries by volume, database and table name.
func tableMap(tables []v1beta1.TableBaseline) map[string]v1beta1.TableBaseline {
	index := make(map[string]v1beta1.TableBaseline, len(tables))
	for _, table := range tables {
		index[table.Volume+"/"+table.Database+"."+table.Name] = table
	}
	return index
}

// DescribeDiffs renders the reason a failed drill or restore reports; the item
// names and both values stay readable in the object's status (C-42).
func DescribeDiffs(diffs []v1beta1.BackupDiff) string {
	parts := make([]string, 0, len(diffs))
	for _, diff := range diffs {
		parts = append(parts, fmt.Sprintf("%s %s→%s", diff.Item, orEmpty(diff.Baseline), orEmpty(diff.Observed)))
	}
	return strings.Join(parts, "；")
}

// orEmpty renders a missing value as a dash.
func orEmpty(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
