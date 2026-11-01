// Package nodehealth reads one node's readiness and dates its loss. C-38 fixes
// one confirmation window for every node-local binding: the kubelet heartbeat
// grace period plus five minutes. Inside the window the binding cannot be
// observed; beyond it the node is confirmed lost.
package nodehealth

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// DefaultGrace is the kubelet heartbeat grace period the confirmation window
// adds five minutes to (C-38).
const DefaultGrace = 40 * time.Second

// ConfirmationWindow is how long a node-local binding may be unobservable
// before it is lost (C-38).
const ConfirmationWindow = 5 * time.Minute

// Observation is one node's readiness as a binding sees it.
type Observation struct {
	// IsReady reports a Ready=True condition.
	IsReady bool
	// IsUnobservable reports a node whose state cannot be read: no Ready
	// condition was ever written, or the node is unready inside the
	// confirmation window.
	IsUnobservable bool
	// IsLost reports a node unready beyond the confirmation window. A node
	// object that does not exist is reported by the caller as lost.
	IsLost bool
	// Since is the last sign of life the loss is dated from; zero when the
	// condition carries no timestamp.
	Since time.Time
}

// Observe reads a node's Ready condition. grace is the heartbeat grace period;
// a non-positive value selects DefaultGrace.
func Observe(node *unstructured.Unstructured, now time.Time, grace time.Duration) Observation {
	if grace <= 0 {
		grace = DefaultGrace
	}
	if node == nil {
		return Observation{IsUnobservable: true}
	}
	conditions, _, _ := unstructured.NestedSlice(node.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["type"] != "Ready" {
			continue
		}
		if condition["status"] == "True" {
			return Observation{IsReady: true}
		}
		since := timestampOf(condition["lastHeartbeatTime"])
		if since.IsZero() {
			since = timestampOf(condition["lastTransitionTime"])
		}
		if since.IsZero() {
			// The loss cannot be dated, so the confirmation window cannot run:
			// the binding is unobservable, never silently healthy (C-07).
			return Observation{IsUnobservable: true}
		}
		if now.Sub(since) > grace+ConfirmationWindow {
			return Observation{IsLost: true, Since: since}
		}
		return Observation{IsUnobservable: true, Since: since}
	}
	return Observation{IsUnobservable: true}
}

// timestampOf parses one RFC3339 node condition timestamp.
func timestampOf(value any) time.Time {
	text, _ := value.(string)
	if text == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
