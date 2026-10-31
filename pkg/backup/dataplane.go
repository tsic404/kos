package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/tsic404/kos/api/v1beta1"
)

// Run layout constants. A run directory holds one copy of every covered volume
// under DataDir and the manifest C-42 compares against under ManifestFile.
const (
	// DataDir is the run subdirectory the volume copies are written to.
	DataDir = "data"
	// DrillDir is the run subdirectory a drill restores into.
	DrillDir = "drill"
	// ManifestFile names the record a run writes.
	ManifestFile = "manifest.json"
)

// Source names one volume's data: the namespace and the volume object name.
type Source struct {
	Namespace string
	Volume    string
}

// Copy names one restored volume: the volume name recorded in the run and the
// directory it is written to.
type Copy struct {
	Recorded    string
	Destination string
}

// DataPlane moves the bytes of a backup (C-41). Capture copies the sources into
// one run directory and measures the copy; Restore writes the recorded volumes
// into their destinations.
type DataPlane interface {
	// Locate returns the directory one volume's data lives in.
	Locate(source Source) string
	// Capture clears the run directory, copies the sources into it and returns
	// the baseline of the copy (C-42). Clearing keeps a resubmitted action
	// from merging an interrupted earlier copy into the baseline (C-06).
	Capture(ctx context.Context, run string, sources []Source) (v1beta1.BackupBaseline, error)
	// Restore writes the recorded volumes into their destinations, clearing a
	// destination first so a retried phase produces the same result (C-06).
	Restore(ctx context.Context, run string, copies []Copy) error
}

// DirDataPlane is the reference data plane: each volume's data lives under
// Root/<namespace>/<volume>, and a run keeps a copy of every covered volume
// under its own data directory.
type DirDataPlane struct {
	// Root is the directory the volume data is reachable under.
	Root string
}

// Locate implements DataPlane.
func (p DirDataPlane) Locate(source Source) string {
	return filepath.Join(p.Root, source.Namespace, source.Volume)
}

// Capture implements DataPlane. The run directory is cleared first, so a
// resubmitted action after a restart cannot merge a half-finished earlier copy
// into the baseline (C-06).
func (p DirDataPlane) Capture(ctx context.Context, run string, sources []Source) (v1beta1.BackupBaseline, error) {
	if err := os.RemoveAll(run); err != nil {
		return v1beta1.BackupBaseline{}, err
	}
	copied := make([]VolumeDir, 0, len(sources))
	for _, source := range sources {
		destination := filepath.Join(run, DataDir, source.Volume)
		if err := copyTree(ctx, p.Locate(source), destination); err != nil {
			return v1beta1.BackupBaseline{}, fmt.Errorf("copy volume %s: %w", source.Volume, err)
		}
		copied = append(copied, VolumeDir{Volume: source.Volume, Path: destination})
	}
	return Measure(copied)
}

// Restore implements DataPlane. Clearing the destination keeps a retried phase
// idempotent; the preflight refuses an existing instance before any data moves,
// so a restore never overwrites an object (C-06, C-41).
func (p DirDataPlane) Restore(ctx context.Context, run string, copies []Copy) error {
	for _, mapping := range copies {
		source := filepath.Join(run, DataDir, mapping.Recorded)
		if err := os.RemoveAll(mapping.Destination); err != nil {
			return err
		}
		if err := copyTree(ctx, source, mapping.Destination); err != nil {
			return fmt.Errorf("restore volume %s: %w", mapping.Recorded, err)
		}
	}
	return nil
}

// copyTree copies the regular files below source into destination. Directories
// are created 0700 and files 0600, so a copy is not more readable than the
// backup target.
func copyTree(ctx context.Context, source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return copyFile(path, target)
	})
}

// copyFile writes one file, replacing a partial copy from an earlier attempt.
func copyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

// Manifest is what one run records: the covered objects, the consistency level
// and the baseline (C-41, C-42). A restore reads it instead of the source
// objects, because a restored instance does not depend on them still existing.
type Manifest struct {
	// Policy is the policy the run belongs to.
	Policy string `json:"policy"`
	// Namespace is the policy's namespace.
	Namespace string `json:"namespace"`
	// CreatedAt is when the run was taken.
	CreatedAt time.Time `json:"createdAt"`
	// Consistency is the level the run reached (C-41).
	Consistency v1beta1.BackupConsistency `json:"consistency"`
	// Applications are the covered applications' specs at backup time.
	Applications []Snapshot `json:"applications,omitempty"`
	// Volumes are the covered volumes' specs at backup time.
	Volumes []Snapshot `json:"volumes,omitempty"`
	// Baseline is the four items the run recorded (C-42).
	Baseline v1beta1.BackupBaseline `json:"baseline"`
}

// Snapshot is one covered object's spec at backup time.
type Snapshot struct {
	// Name is the object name.
	Name string `json:"name"`
	// Spec is the spec the object carried.
	Spec map[string]any `json:"spec,omitempty"`
}

// WriteManifest writes the run record. The file is published by rename, so an
// interrupted write never leaves a manifest a restore would trust.
func WriteManifest(run string, manifest Manifest) error {
	if err := os.MkdirAll(run, 0o700); err != nil {
		return err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	path := filepath.Join(run, ManifestFile)
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// ReadManifest reads the run record; a missing or unreadable manifest is the
// reason a restore stops in Preflight.
func ReadManifest(run string) (Manifest, error) {
	encoded, err := os.ReadFile(filepath.Join(run, ManifestFile))
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("read manifest of %s: %w", run, err)
	}
	return manifest, nil
}
