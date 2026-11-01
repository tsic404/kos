package filemanager

import (
	"path/filepath"
	"testing"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/write"
)

// TestParseExposedVolumes checks what one account may expose.
func TestParseExposedVolumes(t *testing.T) {
	if refs, err := ParseExposedVolumes(""); err != nil || refs != nil {
		t.Fatalf("empty value = %v, %v; want nothing exposed", refs, err)
	}
	refs, err := ParseExposedVolumes(`["apps/docs","team/shared"]`)
	if err != nil {
		t.Fatalf("parse two volumes: %v", err)
	}
	if len(refs) != 2 || refs[0] != (VolumeRef{Namespace: "apps", Name: "docs"}) {
		t.Fatalf("refs = %+v, want the two declared volumes", refs)
	}
	for name, value := range map[string]string{
		"not json":    `apps/docs`,
		"no slash":    `["docs"]`,
		"empty name":  `["apps/"]`,
		"empty scope": `["/docs"]`,
		"nested path": `["apps/group/docs"]`,
	} {
		if _, err := ParseExposedVolumes(value); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// TestCleanPathRefusesEscapes checks that a request can never address anything
// outside the volume root.
func TestCleanPathRefusesEscapes(t *testing.T) {
	accepted := map[string]string{
		"":        "",
		"notes":   "notes",
		"a/b":     "a/b",
		"a//b":    "a/b",
		"a/./b":   "a/b",
		"dir/sub": "dir/sub",
	}
	for raw, want := range accepted {
		cleaned, ok := cleanPath(raw)
		if !ok || cleaned != want {
			t.Fatalf("cleanPath(%q) = %q, %v; want %q", raw, cleaned, ok, want)
		}
	}
	for _, raw := range []string{"/etc/passwd", "..", "../etc", "a/../../etc", "a/..", "a\x00b"} {
		if _, ok := cleanPath(raw); ok {
			t.Fatalf("cleanPath(%q) was accepted", raw)
		}
	}
}

// TestDirectoryRootsRefusesTraversal checks that a volume name cannot step out
// of the base directory.
func TestDirectoryRootsRefusesTraversal(t *testing.T) {
	roots := DirectoryRoots{Base: "/volumes"}
	for _, name := range []string{"..", ".", "", "a/b", `a\b`} {
		if root, ok := roots.Root("apps", name); ok {
			t.Fatalf("volume name %q resolved to %q", name, root)
		}
	}
	if root, ok := roots.Root("apps", "docs"); !ok || root != filepath.Join("/volumes", "apps", "docs") {
		t.Fatalf("the volume root = %q, %v", root, ok)
	}
	if _, ok := (DirectoryRoots{}).Root("apps", "docs"); ok {
		t.Fatal("a data plane without a base served a volume")
	}
}

// TestOwnerAllows checks the ownership rule: the global Owner manages every
// volume, a member its own, and a volume without an owner only the Owner.
func TestOwnerAllows(t *testing.T) {
	owned := &v1beta1.Volume{Spec: v1beta1.VolumeSpec{Owner: "alice"}}
	ownerless := &v1beta1.Volume{}
	cases := []struct {
		name    string
		session identity.Session
		volume  *v1beta1.Volume
		wanted  bool
	}{
		{"global owner", identity.Session{Username: "owner", Role: write.RoleOwner}, owned, true},
		{"global owner on an ownerless volume", identity.Session{Username: "owner", Role: write.RoleOwner}, ownerless, true},
		{"the volume's owner", identity.Session{Username: "alice", Role: write.RoleMember}, owned, true},
		{"another member", identity.Session{Username: "bob", Role: write.RoleMember}, owned, false},
		{"a guest", identity.Session{Username: "guest", Role: write.RoleGuest}, owned, false},
		{"a member on an ownerless volume", identity.Session{Username: "alice", Role: write.RoleMember}, ownerless, false},
	}
	for _, test := range cases {
		if observed := ownerAllows(test.session, test.volume); observed != test.wanted {
			t.Fatalf("%s allowed = %v, want %v", test.name, observed, test.wanted)
		}
	}
}

// TestIsPathSegment keeps the directory mapping to plain names.
func TestIsPathSegment(t *testing.T) {
	for _, value := range []string{"apps", "alice-docs", "a.b"} {
		if !isPathSegment(value) {
			t.Fatalf("%q was refused", value)
		}
	}
	for _, value := range []string{"", ".", "..", "a/b", `a\b`} {
		if isPathSegment(value) {
			t.Fatalf("%q was accepted", value)
		}
	}
}
