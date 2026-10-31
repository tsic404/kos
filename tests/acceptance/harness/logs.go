package harness

import (
	"context"
	"sort"
	"time"

	"github.com/tsic404/kos/pkg/controlplane"
)

// FixtureLogs is a log source over declared lines (C-64). The cluster's log
// endpoint belongs to a kubelet, which an API-server-only test cluster does not
// run, so a case declares the lines the workloads wrote instead.
type FixtureLogs struct {
	lines map[string][]controlplane.LogLine
}

// NewFixtureLogs starts an empty log source.
func NewFixtureLogs() *FixtureLogs {
	return &FixtureLogs{lines: map[string][]controlplane.LogLine{}}
}

// Add declares one line one container wrote at one time.
func (f *FixtureLogs) Add(namespace, pod, container string, at time.Time, message string) {
	key := namespace + "/" + pod + "/" + container
	f.lines[key] = append(f.lines[key], controlplane.LogLine{Timestamp: at, Message: message})
}

// Lines implements controlplane.LogSource.
func (f *FixtureLogs) Lines(_ context.Context, namespace, pod, container string, since time.Time, limit int) ([]controlplane.LogLine, error) {
	key := namespace + "/" + pod + "/" + container
	owned := make([]controlplane.LogLine, 0, len(f.lines[key]))
	for _, line := range f.lines[key] {
		if line.Timestamp.Before(since) {
			continue
		}
		owned = append(owned, line)
	}
	sort.SliceStable(owned, func(left, right int) bool {
		return owned[left].Timestamp.Before(owned[right].Timestamp)
	})
	if len(owned) > limit {
		owned = owned[len(owned)-limit:]
	}
	return owned, nil
}
