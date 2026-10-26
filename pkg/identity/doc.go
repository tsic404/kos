// Package identity is the control plane's account, session and database layer.
//
// It owns the local accounts the browser signs in with (C-63), the sessions
// those logins create, and the control-plane database that holds users,
// sessions, preferences and audit (C-61). The cluster stays authoritative for
// everything else: losing this database stops logins and writes but never
// stops reconciliation (C-61).
package identity
