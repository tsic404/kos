// Package install implements the installation forms of C-54 that touch one
// cluster: installing into an existing cluster and bootstrapping a bare host,
// plus the uninstall and rescue paths that follow an installation.
//
// The install invariant of C-15 and the injection list of C-58 live in the
// install path itself: a run takes a snapshot before and after, reads the API
// server audit log between them and reports what it measured. An install whose
// audit cannot be read reports an unmeasured window instead of claiming the
// promise held (ADR-017).
package install
