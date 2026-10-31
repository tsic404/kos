// Package backup implements the backup policy reconciler and the restore
// reconciler: it records the four baseline items C-42 compares, takes scheduled
// copies off a refused failure domain, and restores a run to a new instance.
//
// The bytes move through a DataPlane. The kernel ships DirDataPlane, which reads
// the volume data the control plane mounts at a directory root; a cluster that
// mounts the volumes into a job supplies another implementation of the same
// interface.
package backup
