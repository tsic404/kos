// Package diagnostics assembles the evidence chain of C-26 and exports the
// diagnostic bundle C-26 and C-56 require.
//
// The chain walks one reference path: application, endpoint, route, backend,
// workload, node, then the extension classes the application renders. Every
// evidence entry carries the shape C-26 fixes and states whether it is an
// observed fact or an inference. A segment whose diagnostic adapter is not
// active stays in the chain marked unobservable, so one missing adapter never
// hides the rest of the path.
package diagnostics
