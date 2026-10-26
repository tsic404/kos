package controlplane

import (
	"testing"
)

// TestPageTokenResumesWithinOneQuery checks that a cursor resumes after the last
// item and refuses to move to a different query (C-12 pagination).
func TestPageTokenResumesWithinOneQuery(t *testing.T) {
	projections := []Projection{
		{Ref: ObjectRef{Group: "kos.tsic.top", Kind: "Application", Namespace: "default", Name: "alpha"}},
		{Ref: ObjectRef{Group: "kos.tsic.top", Kind: "Application", Namespace: "default", Name: "beta"}},
		{Ref: ObjectRef{Group: "kos.tsic.top", Kind: "Application", Namespace: "default", Name: "gamma"}},
	}
	sortProjections(projections)

	applicationType := kernelTypeOf(t, "Application")
	token := encodePageToken(applicationType, "default", "app=web", "", projections[0].Ref.String())
	last, failure := decodePageToken(token, applicationType, "default", "app=web", "")
	if failure != nil {
		t.Fatalf("decode token: %v", failure)
	}
	if position, ok := pagePosition(projections, last); !ok || position != 1 {
		t.Fatalf("position = %d (ok %v), want 1", position, ok)
	}

	// A cursor belongs to one query: another kind, namespace or filter must not
	// resume it, or objects would be skipped without a word.
	otherQueries := []struct {
		name      string
		object    ObjectType
		namespace string
		selector  string
		targetApp string
	}{
		{name: "another kind", object: kernelTypeOf(t, "Volume"), namespace: "default", selector: "app=web"},
		{name: "another namespace", object: applicationType, namespace: "other", selector: "app=web"},
		{name: "another filter", object: applicationType, namespace: "default", selector: "app=api"},
		{name: "another version", object: ObjectType{Group: "kos.tsic.top", Version: "v1", Kind: "Application", Resource: "applications"}, namespace: "default", selector: "app=web"},
		{name: "another target application", object: applicationType, namespace: "default", selector: "app=web", targetApp: "web"},
	}
	for _, query := range otherQueries {
		t.Run(query.name, func(t *testing.T) {
			if _, failure := decodePageToken(token, query.object, query.namespace, query.selector, query.targetApp); failure == nil {
				t.Fatal("the cursor was accepted by another query")
			} else if failure.Code != CodeInvalidField {
				t.Fatalf("code = %s, want %s", failure.Code, CodeInvalidField)
			}
		})
	}
}

// TestPagePositionRejectsCursorsOutsideTheListing checks that a cursor sorting
// before the first item is refused instead of silently replaying the listing,
// while a cursor past the end still ends the walk.
func TestPagePositionRejectsCursorsOutsideTheListing(t *testing.T) {
	projections := []Projection{
		{Ref: ObjectRef{Group: "kos.tsic.top", Kind: "Application", Namespace: "default", Name: "beta"}},
		{Ref: ObjectRef{Group: "kos.tsic.top", Kind: "Application", Namespace: "default", Name: "gamma"}},
	}
	sortProjections(projections)

	if _, ok := pagePosition(projections, "kos.tsic.top/Application/default/alpha"); ok {
		t.Fatal("a cursor before the first item must be rejected")
	}
	if position, ok := pagePosition(projections, "kos.tsic.top/Application/default/zzz"); !ok || position != len(projections) {
		t.Fatalf("position past the end = %d (ok %v), want %d and ok", position, ok, len(projections))
	}
	if position, ok := pagePosition(projections, ""); !ok || position != 0 {
		t.Fatalf("position without a cursor = %d (ok %v), want 0 and ok", position, ok)
	}
	if position, ok := pagePosition(nil, "kos.tsic.top/Application/default/alpha"); !ok || position != 0 {
		t.Fatalf("position over an empty listing = %d (ok %v), want 0 and ok", position, ok)
	}
}

// TestPageSizeBounds checks the page size limits.
func TestPageSizeBounds(t *testing.T) {
	if size, failure := pageSize(""); failure != nil || size != defaultPageSize {
		t.Fatalf("empty limit = %d, %v; want %d, nil", size, failure, defaultPageSize)
	}
	if size, failure := pageSize("7"); failure != nil || size != 7 {
		t.Fatalf("limit 7 = %d, %v", size, failure)
	}
	for _, raw := range []string{"0", "-1", "abc", "501"} {
		if _, failure := pageSize(raw); failure == nil || failure.Code != CodeInvalidField {
			t.Fatalf("limit %q = %v, want an invalid_field failure", raw, failure)
		}
	}
}

// TestParseSelectorRejectsMalformedInput checks the filter parameter.
func TestParseSelectorRejectsMalformedInput(t *testing.T) {
	selector, failure := parseSelector("app=web,tier in (front,edge)")
	if failure != nil {
		t.Fatalf("parse selector: %v", failure)
	}
	if selector.String() == "" {
		t.Fatal("a parsed selector must render")
	}
	if _, failure := parseSelector("app in ("); failure == nil || failure.Code != CodeInvalidField {
		t.Fatalf("malformed selector = %v, want an invalid_field failure", failure)
	}
}
