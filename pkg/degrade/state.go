package degrade

import (
	"sync"
	"time"
)

// Notice kinds the shell displays as a banner (C-56).
const (
	// NoticeReadOnly is the global read-only banner.
	NoticeReadOnly = "read-only"
	// NoticeStale marks reads served from a cache.
	NoticeStale = "stale"
	// NoticeUnavailable marks the cluster API refusing reads before any cache
	// was filled.
	NoticeUnavailable = "unavailable"
)

// Notice is one shell-level banner. The panel shows it above every page, so a
// degradation is visible from wherever the user is (C-56).
type Notice struct {
	// Kind is one of the notice kinds.
	Kind string `json:"kind"`
	// Message is the sentence the banner shows.
	Message string `json:"message"`
	// Reason names the cause; empty when the kind says everything.
	Reason string `json:"reason,omitempty"`
	// Since is when the state began.
	Since time.Time `json:"since"`
}

// State is the process-wide degradation state (C-56). A component that owns one
// failure class writes the fact here; the API and the shell only read it.
type State struct {
	// IsReadOnly reports that writes are refused cluster-wide.
	IsReadOnly bool `json:"readOnly"`
	// Notice is the banner the shell shows; nil when nothing is degraded.
	Notice *Notice `json:"notice,omitempty"`
}

// StateSource reports the current process-wide degradation state.
type StateSource interface {
	State() State
}

// Static is a state a deployment sets as it observes the cluster. It is safe
// for concurrent readers.
type Static struct {
	mu    sync.RWMutex
	state State
}

// State implements StateSource.
func (s *Static) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state.Notice == nil {
		return State{IsReadOnly: s.state.IsReadOnly}
	}
	notice := *s.state.Notice
	return State{IsReadOnly: s.state.IsReadOnly, Notice: &notice}
}

// EnterReadOnly records that the cluster stopped accepting writes (C-56). A
// second call keeps the original start time, so the banner does not flicker.
func (s *Static) EnterReadOnly(reason string, since time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.IsReadOnly {
		return
	}
	s.state = State{
		IsReadOnly: true,
		Notice: &Notice{
			Kind:    NoticeReadOnly,
			Message: "集群处于只读状态，写操作被拒绝",
			Reason:  reason,
			Since:   since,
		},
	}
}

// ClearReadOnly removes the read-only state once writes are accepted again.
func (s *Static) ClearReadOnly() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = State{}
}
