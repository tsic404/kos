package backup

import (
	"testing"
)

// TestWithinDirectory covers the restore read boundary: only a run below the
// policy's backup target may be read, so a status path cannot be pointed at
// another location (C-41).
func TestWithinDirectory(t *testing.T) {
	root := "/srv/backups"
	cases := []struct {
		path string
		want bool
	}{
		{"/srv/backups/apps/nightly/20261031T030000Z", true},
		{"/srv/backups", true},
		{"/srv/backups-evil/run", false},
		{"/srv/backups/../etc", false},
		{"/etc", false},
		{"", false},
	}
	for _, test := range cases {
		if got := withinDirectory(root, test.path); got != test.want {
			t.Errorf("withinDirectory(%q, %q) = %v, want %v", root, test.path, got, test.want)
		}
	}
	if withinDirectory("", "/srv/backups/run") {
		t.Error("an empty root accepted a path")
	}
}
