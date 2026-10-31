// Package controlplane serves the only read and write surface browsers and
// extensions get (C-12): domain objects as resources, projections that carry
// their own observation time (C-13), and plan requests that change nothing
// (C-62).
//
// The read-time surfaces of C-64 live here too: one application's logs and the
// recent events. Both are read when the request runs, stored nowhere, and
// bounded by a line and a time window.
//
// The API never proxies the Kubernetes API and never gives a client cluster
// credentials: it holds the client and exposes only the ten kernel kinds. Within
// one major version fields are only added; a breaking change needs a new major.
package controlplane
