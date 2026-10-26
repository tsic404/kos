// Package endpoint implements the five endpoint shapes (C-43), the allocation
// ledger that makes them unique (C-44) and the application/endpoint relation
// (C-45).
//
// The shape matrix is enforced twice from one validator: the control plane
// rejects an illegal write before it reaches the cluster, and the reconciler
// marks an object that bypassed the API instead of admitting it. Uniqueness is
// the reconciler's alone: a precheck at the API is a hint, so a duplicate
// created around it is detected here (C-44).
package endpoint

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/write"
)

// PathPrefixCapability is the baseline capability an http.path endpoint needs
// (C-09, C-43).
const PathPrefixCapability = "network.path-prefix"

// Facts are the two declarations C-43 requires before an http.path endpoint may
// exist: the application's package declares path-prefix use safe, and the
// cluster registers the path-prefix capability. The kernel keeps them on the
// application definition (C-32) and the Provider registration (C-47); this
// package reads them through one seam so endpoint admission does not hard-code
// the field layout of two other contracts.
type Facts interface {
	// PathSafe reports whether the package of app declares path-prefix use
	// safe.
	PathSafe(ctx context.Context, app write.Ref) (bool, error)
	// CapabilityRegistered reports whether the cluster registered capability.
	CapabilityRegistered(ctx context.Context, capability string) (bool, error)
}

// FieldError is a rejection that names the spec field it is about (C-43).
type FieldError struct {
	// Field is the spec path, such as spec.host.
	Field string
	// Reason explains what is wrong with that field.
	Reason string
}

// Error renders the field and its reason together.
func (e *FieldError) Error() string {
	return e.Field + " " + e.Reason
}

// field builds a rejection of one spec field.
func field(name, reason string) *FieldError {
	return &FieldError{Field: name, Reason: reason}
}

// fieldf builds a rejection whose reason is formatted.
func fieldf(name, format string, args ...any) *FieldError {
	return &FieldError{Field: name, Reason: fmt.Sprintf(format, args...)}
}

// SpecOf reads the typed spec of an endpoint object.
func SpecOf(object *unstructured.Unstructured) (v1beta1.EndpointSpec, error) {
	var spec v1beta1.EndpointSpec
	raw, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil {
		return spec, fmt.Errorf("read endpoint spec: %w", err)
	}
	if !found {
		return spec, nil
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &spec); err != nil {
		return spec, fmt.Errorf("decode endpoint spec: %w", err)
	}
	return spec, nil
}

// Validate applies C-43's field matrix to one endpoint spec. The namespace is
// the endpoint's, so the target application resolves the way C-43 says it must.
//
// A nil Facts means neither declaration is readable, and C-43 then refuses the
// path-prefix shape: ADR-008 keeps it off until both are declared.
func Validate(ctx context.Context, namespace string, spec v1beta1.EndpointSpec, facts Facts) *FieldError {
	if spec.TargetApp == "" {
		return field("spec.targetApp", "必填")
	}
	switch spec.Kind {
	case v1beta1.EndpointKindHTTPHost:
		if failure := checkHost(spec.Host); failure != nil {
			return failure
		}
		if spec.Path != "" {
			return field("spec.path", "在 http.host 形态中禁止")
		}
		if spec.Port != 0 {
			return field("spec.port", "在 http.host 形态中禁止")
		}
		if spec.ServiceName != "" {
			return field("spec.serviceName", "在 http.host 形态中禁止")
		}
		return nil
	case v1beta1.EndpointKindHTTPPath:
		if failure := checkHost(spec.Host); failure != nil {
			return failure
		}
		if spec.Path == "" {
			return field("spec.path", "必填")
		}
		if !strings.HasPrefix(spec.Path, "/") {
			return field("spec.path", "必须以 / 开头")
		}
		if spec.Port != 0 {
			return field("spec.port", "在 http.path 形态中禁止")
		}
		if spec.ServiceName != "" {
			return field("spec.serviceName", "在 http.path 形态中禁止")
		}
		return checkPathPrefix(ctx, namespace, spec, facts)
	case v1beta1.EndpointKindTCP, v1beta1.EndpointKindUDP:
		if failure := checkPort(spec.Port); failure != nil {
			return failure
		}
		if spec.Host != "" {
			return field("spec.host", "在 "+string(spec.Kind)+" 形态中禁止")
		}
		if spec.Path != "" {
			return field("spec.path", "在 "+string(spec.Kind)+" 形态中禁止")
		}
		if spec.ServiceName != "" {
			return field("spec.serviceName", "在 "+string(spec.Kind)+" 形态中禁止")
		}
		if spec.TLS != nil {
			return field("spec.tls", "在 "+string(spec.Kind)+" 形态中禁止")
		}
		if spec.Auth != nil {
			return field("spec.auth", "在 "+string(spec.Kind)+" 形态中禁止")
		}
		return nil
	case v1beta1.EndpointKindMDNS:
		if failure := checkPort(spec.Port); failure != nil {
			return failure
		}
		if spec.ServiceName == "" {
			return field("spec.serviceName", "必填")
		}
		if spec.Host != "" {
			return field("spec.host", "在 mdns 形态中禁止")
		}
		if spec.Path != "" {
			return field("spec.path", "在 mdns 形态中禁止")
		}
		if spec.TLS != nil {
			return field("spec.tls", "在 mdns 形态中禁止")
		}
		if spec.Auth != nil {
			return field("spec.auth", "在 mdns 形态中禁止")
		}
		return nil
	default:
		return fieldf("spec.kind", "取值 %q 不是 http.host、http.path、l4.tcp、l4.udp 或 mdns", spec.Kind)
	}
}

// checkHost requires a hostname a URL can carry.
func checkHost(host string) *FieldError {
	if host == "" {
		return field("spec.host", "必填")
	}
	if problems := validation.IsDNS1123Subdomain(CanonicalHost(host)); len(problems) > 0 {
		return fieldf("spec.host", "不是合法主机名：%s", problems[0])
	}
	return nil
}

// checkPort requires a port in the allocatable range. The schema carries the
// same bounds, so a struct built in code cannot skip them.
func checkPort(port int32) *FieldError {
	if port == 0 {
		return field("spec.port", "必填")
	}
	if port < 1 || port > 65535 {
		return field("spec.port", "必须在 1 到 65535 之间")
	}
	return nil
}

// checkPathPrefix asks the two declarations C-43 requires, naming the missing
// side so a write learns which one to fix (AC-38).
func checkPathPrefix(ctx context.Context, namespace string, spec v1beta1.EndpointSpec, facts Facts) *FieldError {
	app := write.Ref{Group: api.Group, Version: v1beta1.GroupVersion.Version, Kind: "Application", Namespace: namespace, Name: spec.TargetApp}
	if facts == nil {
		return fieldf("spec.kind", "http.path 需要 %s 能力且应用 %s 的包声明可安全使用路径前缀，当前两者都未声明",
			PathPrefixCapability, spec.TargetApp)
	}
	capable, err := facts.CapabilityRegistered(ctx, PathPrefixCapability)
	if err != nil {
		return fieldf("spec.kind", "读取 %s 能力失败：%v", PathPrefixCapability, err)
	}
	if !capable {
		return fieldf("spec.kind", "http.path 需要 %s 能力，当前未登记", PathPrefixCapability)
	}
	safe, err := facts.PathSafe(ctx, app)
	if err != nil {
		return fieldf("spec.path", "读取应用 %s 的包声明失败：%v", app.Name, err)
	}
	if !safe {
		return fieldf("spec.path", "应用 %s 的包未声明可安全使用路径前缀", app.Name)
	}
	return nil
}

// CanonicalHost lowercases a hostname and drops its trailing dot, so two
// spellings of one name share one ledger key (C-44).
func CanonicalHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// Key is one allocation entry of the ledger (C-44). Two shapes that share a
// space and a value contend for one key; TCP and UDP are separate spaces, as
// are hostname and hostname+path.
type Key struct {
	// Space is the key space: host, path, tcp, udp or mdns.
	Space string `json:"space"`
	// Value is the allocated value inside that space.
	Value string `json:"value"`
}

// String renders the key the way conditions name it.
func (k Key) String() string {
	return k.Space + "/" + k.Value
}

// Key spaces of the ledger (C-44).
const (
	SpaceHost = "host"
	SpacePath = "path"
	SpaceTCP  = "tcp"
	SpaceUDP  = "udp"
	SpaceMDNS = "mdns"
)

// KeysOf returns the keys one valid spec holds (C-44). An illegal shape holds
// none: it is never admitted, so it never allocates.
func KeysOf(spec v1beta1.EndpointSpec) []Key {
	switch spec.Kind {
	case v1beta1.EndpointKindHTTPHost:
		return []Key{{Space: SpaceHost, Value: CanonicalHost(spec.Host)}}
	case v1beta1.EndpointKindHTTPPath:
		return []Key{{Space: SpacePath, Value: CanonicalHost(spec.Host) + spec.Path}}
	case v1beta1.EndpointKindTCP:
		return []Key{{Space: SpaceTCP, Value: fmt.Sprint(spec.Port)}}
	case v1beta1.EndpointKindUDP:
		return []Key{{Space: SpaceUDP, Value: fmt.Sprint(spec.Port)}}
	case v1beta1.EndpointKindMDNS:
		return []Key{{Space: SpaceMDNS, Value: spec.ServiceName + ":" + fmt.Sprint(spec.Port)}}
	default:
		return nil
	}
}

// TargetsApp reports whether an endpoint exposes app (C-45). It is the one
// predicate both the reverse lookup and the cascade use, so the set the panel
// shows and the set a deletion touches cannot drift.
func TargetsApp(object *unstructured.Unstructured, app string) bool {
	spec, err := SpecOf(object)
	if err != nil {
		return false
	}
	return spec.TargetApp == app
}
