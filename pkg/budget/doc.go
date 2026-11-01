// Package budget carries the resource budgets of C-57 and the measurement that
// judges them: the steady and install-peak control-plane budgets, the per-node
// collector budget, the database disk cap and the two latency targets. A report
// evaluates measured values against the declared limits; a real three-node
// retest is judged against one and a half times the declared limits.
package budget
