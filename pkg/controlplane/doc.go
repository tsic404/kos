// Package controlplane serves the only read and write surface browsers and
// extensions get (C-12): domain objects as resources, projections that carry
// their own observation time (C-13), and plan requests that change nothing
// (C-62).
//
// The API never proxies the Kubernetes API and never gives a client cluster
// credentials: it holds the client and exposes only the ten kernel kinds. Within
// one major version fields are only added; a breaking change needs a new major.
package controlplane
