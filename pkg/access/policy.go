// Package access is the rule set behind the control plane's authorization seam:
// which objects a session role sees in the panel, which it may write, and who
// owns the objects it writes (C-59, C-60).
//
// The rules are evaluated per request. A role is never cached on the session
// record here: the caller passes the role the identity middleware resolved for
// this request, so a demotion takes effect on the next call (C-63).
package access

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/write"
)

// Session is the identity behind one request: the account name and the role
// read for this call (C-59, C-63).
type Session struct {
	// Username is the login name; it is what spec.owner and the visibility
	// member list carry.
	Username string
	// Role is the account's current role.
	Role write.Role
}

// signedIn reports whether the session carries a usable role.
func (s Session) signedIn() bool {
	return s.Username != "" && (s.Role == write.RoleOwner || s.Role == write.RoleMember || s.Role == write.RoleGuest)
}

// isOwner reports whether the session is the global Owner, which writes every
// object and is the only role that transfers one (C-59, C-60).
func (s Session) isOwner() bool { return s.Role == write.RoleOwner }

// memberKinds are the kinds a Member may write, and only when it owns them
// (C-59). An endpoint's owner is its target application's, resolved through the
// application rather than a field of its own (C-60).
var memberKinds = map[string]bool{
	"Application":     true,
	"Endpoint":        true,
	"Volume":          true,
	"BackupPolicy":    true,
	"RestoreJob":      true,
	"VolumeMigration": true,
}

// ownedKinds carry spec.owner; the list is C-60's and the write path's single
// source for which kinds the API stamps.
var ownedKinds = map[string]bool{
	"Application":     true,
	"Volume":          true,
	"BackupPolicy":    true,
	"RestoreJob":      true,
	"VolumeMigration": true,
}

// ownerOnlyKinds are managed by the global Owner alone: a Member's write is
// refused whatever its ownership (C-59).
var ownerOnlyKinds = map[string]bool{
	"Provider":              true,
	"SystemComponent":       true,
	"ApplicationDefinition": true,
	"NodeDevice":            true,
}

// ExtensionRoles answers what an extension's declaration grants (C-18): the
// session roles that may write its object classes, and which extension declares
// a class (C-14). The extension registry implements it.
type ExtensionRoles interface {
	// ObjectWriters returns the declared writer roles of one class; ok is false
	// when no declaration names the class.
	ObjectWriters(group, kind string) (writers []write.Role, ok bool)
	// DeclaredBy returns the identifier of the extension declaring the class.
	DeclaredBy(group, kind string) (identifier string, ok bool)
}

// Policy is the C-59/C-60 rule set over control-plane requests.
type Policy struct {
	// Objects reads the objects an ownership rule resolves through. A nil
	// source makes every cross-object lookup fail closed.
	Objects controlplane.ObjectSource
	// Extensions resolves declared extension write roles; nil means no class is
	// declared, so only the global Owner writes extension objects (C-59).
	Extensions ExtensionRoles
}

// Decide judges one request. The grant carries the actor for the audit stream
// even when the request is refused (C-14), and the owner the API writes when a
// permitted write creates or transfers one (C-60).
func (p *Policy) Decide(ctx context.Context, session Session, request controlplane.Request) (controlplane.Grant, error) {
	grant := controlplane.Grant{Actor: session.Username}
	if !session.signedIn() {
		return grant, fmt.Errorf("access: the request carries no role")
	}
	switch request.Verb {
	case controlplane.VerbRead:
		if request.Object == nil {
			return grant, nil
		}
		visible, err := p.visible(ctx, session, request.Ref, request.Object)
		if err != nil {
			return grant, err
		}
		if !visible {
			return grant, controlplane.ErrHidden
		}
		return grant, nil
	case controlplane.VerbPlan, controlplane.VerbCreate, controlplane.VerbUpdate, controlplane.VerbApply:
		item, err := p.write(ctx, session, request)
		item.Actor = grant.Actor
		return item, err
	case controlplane.VerbDelete:
		return p.remove(ctx, session, request, grant)
	case controlplane.VerbEventRead:
		return grant, p.eventVisible(ctx, session, request)
	default:
		return grant, fmt.Errorf("access: verb %q is not judged", request.Verb)
	}
}

// visible applies C-59's visibility predicate. Only applications have one:
// every other class is visible to every signed-in role.
func (p *Policy) visible(ctx context.Context, session Session, ref controlplane.ObjectRef, object map[string]any) (bool, error) {
	if ref.Group != api.Group || ref.Kind != "Application" {
		return true, nil
	}
	spec, _ := object["spec"].(map[string]any)
	owner, _ := spec["owner"].(string)
	// The object's owner always sees it, and the global Owner manages every
	// application (C-59).
	if session.isOwner() || (owner != "" && owner == session.Username) {
		return true, nil
	}
	visibility, _ := spec["visibility"].(map[string]any)
	if session.Role == write.RoleGuest {
		allow, _ := visibility["allowGuests"].(bool)
		return allow, nil
	}
	// A member sees the application only when the member list names its
	// account. An entry that names no account never matches a signed-in
	// username, so unresolvable names drop out without a lookup (C-59).
	return namesMember(visibility, session.Username), nil
}

// namesMember reports whether a member list names one username.
func namesMember(visibility map[string]any, username string) bool {
	if username == "" {
		return false
	}
	members, _ := visibility["members"].([]any)
	for _, entry := range members {
		if name, _ := entry.(string); name == username {
			return true
		}
	}
	return false
}

// eventVisible applies C-64's event rule on top of C-59: an application's
// events follow the application's visibility, while every other object's events
// reach only the global Owner and the member owning the object, never a Guest.
// An object the read could not resolve counts as one without an owner, so the
// rule never widens what the panel shows.
func (p *Policy) eventVisible(ctx context.Context, session Session, request controlplane.Request) error {
	ref := request.Ref
	if ref.Group == api.Group && ref.Kind == "Application" {
		visible, err := p.visible(ctx, session, ref, request.Object)
		if err != nil {
			return err
		}
		if !visible {
			return controlplane.ErrHidden
		}
		return nil
	}
	if session.isOwner() {
		return nil
	}
	if session.Role == write.RoleGuest {
		return controlplane.ErrHidden
	}
	if owner, has := ownerOf(request.Object); has && owner == session.Username {
		return nil
	}
	return controlplane.ErrHidden
}

// write judges a create, update, apply or plan (C-59, C-60).
func (p *Policy) write(ctx context.Context, session Session, request controlplane.Request) (controlplane.Grant, error) {
	ref := request.Ref
	if ref.Group != api.Group {
		return p.writeExtension(session, ref)
	}
	if session.Role == write.RoleGuest {
		return controlplane.Grant{}, denyf("%s is read-only", session.Role)
	}
	switch {
	case session.isOwner():
		return p.writeOwner(session, request)
	case session.Role == write.RoleMember:
		return p.writeMember(ctx, session, request)
	default:
		return controlplane.Grant{}, denyf("role %q writes nothing", session.Role)
	}
}

// writeOwner judges the global Owner's write. The Owner writes every kind; the
// only rule left is the owner field: the API stamps the session at creation and
// only a transfer may move it after that (C-60).
func (p *Policy) writeOwner(session Session, request controlplane.Request) (controlplane.Grant, error) {
	if !ownedKinds[request.Ref.Kind] {
		return controlplane.Grant{}, nil
	}
	if request.Current == nil {
		return controlplane.Grant{Owner: session.Username}, nil
	}
	current, _ := ownerOf(request.Current)
	if submitted, hasSubmitted := ownerOf(request.Object); hasSubmitted && submitted != "" && submitted != current {
		return controlplane.Grant{Owner: submitted}, nil
	}
	return controlplane.Grant{Owner: current}, nil
}

// writeMember judges a Member's write: it may write only the kinds C-59 gives
// it and only the objects it owns (C-60).
func (p *Policy) writeMember(ctx context.Context, session Session, request controlplane.Request) (controlplane.Grant, error) {
	ref := request.Ref
	switch {
	case !memberKinds[ref.Kind]:
		return controlplane.Grant{}, denyf("only the global Owner may write %s", ref.Kind)
	case ref.Kind == "Endpoint":
		if err := p.requireApplicationOwner(ctx, session, ref, request.Object, request.Current); err != nil {
			return controlplane.Grant{}, err
		}
		return controlplane.Grant{}, nil
	}
	owner := session.Username
	if request.Current != nil {
		current, _ := ownerOf(request.Current)
		if current == "" {
			return controlplane.Grant{}, denyf("an object without an owner is the global Owner's")
		}
		if current != session.Username {
			return controlplane.Grant{}, denyf("the object is owned by %q", current)
		}
		if submitted, hasSubmitted := ownerOf(request.Object); hasSubmitted && submitted != current {
			return controlplane.Grant{}, denyf("only the global Owner may transfer spec.owner")
		}
		owner = current
	}
	if err := p.checkReferences(ctx, session, ref, request.Object); err != nil {
		return controlplane.Grant{}, err
	}
	return controlplane.Grant{Owner: owner}, nil
}

// remove judges a delete. An object that is already gone is still judged by
// role, so a Guest is refused and a repeat delete stays idempotent (C-12).
func (p *Policy) remove(ctx context.Context, session Session, request controlplane.Request, grant controlplane.Grant) (controlplane.Grant, error) {
	ref := request.Ref
	if ref.Group != api.Group {
		item, err := p.writeExtension(session, ref)
		item.Actor = grant.Actor
		return item, err
	}
	if session.Role == write.RoleGuest {
		return grant, denyf("%s is read-only", session.Role)
	}
	if session.isOwner() {
		return grant, nil
	}
	if request.Current == nil {
		// Nothing is stored under the name; there is no ownership to protect.
		return grant, nil
	}
	switch {
	case ref.Kind == "Endpoint":
		if err := p.requireApplicationOwner(ctx, session, ref, nil, request.Current); err != nil {
			return grant, err
		}
		return grant, nil
	case memberKinds[ref.Kind]:
		current, _ := ownerOf(request.Current)
		if current == "" {
			return grant, denyf("an object without an owner is the global Owner's")
		}
		if current != session.Username {
			return grant, denyf("the object is owned by %q", current)
		}
		return grant, nil
	default:
		return grant, denyf("only the global Owner may delete %s", ref.Kind)
	}
}

// writeExtension judges an extension object's write by its declaration (C-18):
// the declared writer roles may write it, and an undeclared class is the global
// Owner's alone (C-59).
func (p *Policy) writeExtension(session Session, ref controlplane.ObjectRef) (controlplane.Grant, error) {
	if p.Extensions == nil {
		if session.isOwner() {
			return controlplane.Grant{}, nil
		}
		return controlplane.Grant{}, denyf("%s writes no object of class %s", session.Role, ref.Kind)
	}
	identifier, _ := p.Extensions.DeclaredBy(ref.Group, ref.Kind)
	grant := controlplane.Grant{Extension: identifier}
	writers, declared := p.Extensions.ObjectWriters(ref.Group, ref.Kind)
	if !declared {
		if session.isOwner() {
			return grant, nil
		}
		return grant, denyf("%s writes no undeclared class %s", session.Role, ref.Kind)
	}
	for _, writer := range writers {
		if writer == session.Role {
			return grant, nil
		}
	}
	return grant, denyf("the declaration does not let %s write %s", session.Role, ref.Kind)
}

// requireApplicationOwner holds an endpoint to its target application's owner:
// an endpoint has no owner of its own, and C-60 derives it from the application
// it exposes.
func (p *Policy) requireApplicationOwner(ctx context.Context, session Session, ref controlplane.ObjectRef, object, current map[string]any) error {
	name := ""
	if current != nil {
		name = nestedString(current, "spec", "targetApp")
	}
	if name == "" && object != nil {
		name = nestedString(object, "spec", "targetApp")
	}
	if name == "" {
		return denyf("the endpoint names no target application")
	}
	owner, found, err := p.ownerOf(ctx, "Application", ref.Namespace, name)
	if err != nil {
		return err
	}
	if !found {
		return denyf("the target application %q does not exist, so its owner cannot be resolved", name)
	}
	if owner != session.Username {
		return denyf("the target application is owned by %q", owner)
	}
	return nil
}

// checkReferences enforces C-60's cross-owner rules on one Member write: an
// application may not reference another account's volume, a backup policy's
// selector may not reach another account's applications or volumes, and a
// restore job may not read another account's policy (C-41). The rules only
// decide range; they never change ownership.
func (p *Policy) checkReferences(ctx context.Context, session Session, ref controlplane.ObjectRef, object map[string]any) error {
	if object == nil {
		return nil
	}
	switch ref.Kind {
	case "Application":
		for _, name := range stringList(nestedValue(object, "spec", "volumes")) {
			owner, found, err := p.ownerOf(ctx, "Volume", ref.Namespace, name)
			if err != nil {
				return err
			}
			if found && owner != "" && owner != session.Username {
				return denyf("volume %q is owned by %q", name, owner)
			}
		}
	case "RestoreJob":
		policyName := nestedString(object, "spec", "policy")
		if policyName == "" {
			return denyf("spec.policy is required: a restore reads one policy's run")
		}
		owner, found, err := p.ownerOf(ctx, "BackupPolicy", ref.Namespace, policyName)
		if err != nil {
			return err
		}
		if !found {
			return denyf("backup policy %q does not exist, so its owner cannot be resolved", policyName)
		}
		if owner != "" && owner != session.Username {
			return denyf("backup policy %q is owned by %q", policyName, owner)
		}
	case "BackupPolicy":
		selector := nestedString(object, "spec", "selector")
		if selector == "" {
			return nil
		}
		parsed, err := labels.Parse(selector)
		if err != nil {
			return denyf("spec.selector is not a label selector: %v", err)
		}
		for _, kind := range []string{"Application", "Volume"} {
			owners, err := p.matchedOwners(ctx, kind, parsed)
			if err != nil {
				return err
			}
			for _, owner := range owners {
				if owner != "" && owner != session.Username {
					return denyf("the selector reaches a %s owned by %q", kind, owner)
				}
			}
		}
	}
	return nil
}

// matchedOwners lists the owners of every object of one kernel kind the selector
// matches.
func (p *Policy) matchedOwners(ctx context.Context, kind string, selector labels.Selector) ([]string, error) {
	if p.Objects == nil {
		return nil, denyf("a selector cannot be judged without a reader")
	}
	objectType, ok := kernelObjectType(kind)
	if !ok {
		return nil, fmt.Errorf("access: %s is not a kernel kind", kind)
	}
	observed, err := p.Objects.List(ctx, objectType, "")
	if err != nil {
		return nil, err
	}
	owners := make([]string, 0, len(observed))
	for _, item := range observed {
		metadata, _ := item.Object["metadata"].(map[string]any)
		objectLabels, _ := metadata["labels"].(map[string]any)
		if !selector.Matches(labels.Set(flatLabels(objectLabels))) {
			continue
		}
		owner, _ := ownerOf(item.Object)
		owners = append(owners, owner)
	}
	return owners, nil
}

// ownerOf reads one kernel object's owner; found is false when the object does
// not exist.
func (p *Policy) ownerOf(ctx context.Context, kind, namespace, name string) (string, bool, error) {
	if p.Objects == nil {
		return "", false, denyf("ownership cannot be resolved without a reader")
	}
	objectType, ok := kernelObjectType(kind)
	if !ok {
		return "", false, fmt.Errorf("access: %s is not a kernel kind", kind)
	}
	observed, err := p.Objects.Get(ctx, objectType, namespace, name)
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	owner, _ := ownerOf(observed.Object)
	return owner, true, nil
}

// kernelObjectType builds the class of one kernel kind in the storage version.
func kernelObjectType(kind string) (controlplane.ObjectType, bool) {
	resource, ok := api.ResourceName(kind)
	if !ok {
		return controlplane.ObjectType{}, false
	}
	scope, ok := api.ScopeOf(kind)
	if !ok {
		return controlplane.ObjectType{}, false
	}
	return controlplane.ObjectType{
		Group:      api.Group,
		Version:    v1beta1.GroupVersion.Version,
		Kind:       kind,
		Resource:   resource,
		Namespaced: scope == api.ScopeNamespaced,
	}, true
}

// ownerOf reads spec.owner of a raw object; has is false when the field is
// absent.
func ownerOf(object map[string]any) (owner string, has bool) {
	value, ok := nestedValue(object, "spec", "owner").(string)
	return value, ok
}

// nestedValue reads a value below a chain of map keys.
func nestedValue(object map[string]any, path ...string) any {
	var current any = object
	for _, key := range path {
		fields, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = fields[key]
	}
	return current
}

// nestedString reads a nested string, treating a missing value as empty.
func nestedString(object map[string]any, path ...string) string {
	value, _ := nestedValue(object, path...).(string)
	return value
}

// stringList reads a list of strings, ignoring entries of another type.
func stringList(value any) []string {
	entries, ok := value.([]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if name, _ := entry.(string); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// flatLabels converts a raw label map for selector matching.
func flatLabels(value map[string]any) map[string]string {
	converted := make(map[string]string, len(value))
	for key, item := range value {
		if text, ok := item.(string); ok {
			converted[key] = text
		}
	}
	return converted
}

// denyf builds a refusal reason; the control plane renders it as the stable
// permission failure and keeps the reason in its log (C-12).
func denyf(format string, args ...any) error {
	return fmt.Errorf("access: "+format, args...)
}
