// Package app implements the application domain of the kernel: the package and
// config structures of C-32, the one-way translation template, the capability
// requirements of C-11, and the lifecycle and retry rules of C-33.
//
// Rendering is a pure function of a definition, a config and a declared
// namespace: it reaches no external service and contains no randomness, so the
// same input produces the same output. All writes go through pkg/write, whose
// tables decide what the application reconciler may create and delete (C-04).
//
// Runtime is the production wiring: it enqueues applications and their action
// records from the object stream and sweeps the records of applications that
// are gone (C-06, C-33, C-69).
package app
