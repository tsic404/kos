package endpoint

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/write"
)

// noFacts is the admission input of a cluster that declares nothing.
type noFacts struct{}

func (noFacts) PathSafe(context.Context, write.Ref) (bool, error) { return false, nil }

func (noFacts) CapabilityRegistered(context.Context, string) (bool, error) { return false, nil }

// facts declares the path-prefix capability and, optionally, the applications
// whose package declares path safety.
type facts struct {
	capable bool
	safe    map[string]bool
	err     error
}

func (f facts) PathSafe(_ context.Context, app write.Ref) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.safe[app.Namespace+"/"+app.Name], nil
}

func (f facts) CapabilityRegistered(context.Context, string) (bool, error) {
	return f.capable, nil
}

// hostSpec, pathSpec and the L4 shapes are the five legal forms C-43 defines.
func hostSpec() v1beta1.EndpointSpec {
	return v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPHost, TargetApp: "web", Host: "app.test"}
}

func pathSpec() v1beta1.EndpointSpec {
	return v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPPath, TargetApp: "web", Host: "app.test", Path: "/web"}
}

func TestValidateAcceptsEveryShape(t *testing.T) {
	declared := facts{capable: true, safe: map[string]bool{"default/web": true}}
	cases := map[string]v1beta1.EndpointSpec{
		"http.host": hostSpec(),
		"http.host with tls and auth": func() v1beta1.EndpointSpec {
			spec := hostSpec()
			spec.TLS = &v1beta1.EndpointTLS{IssuerRef: "internal"}
			spec.Auth = &v1beta1.EndpointAuth{ProviderRef: "sso"}
			return spec
		}(),
		"http.path": pathSpec(),
		"l4.tcp":    {Kind: v1beta1.EndpointKindTCP, TargetApp: "web", Port: 8080},
		"l4.udp":    {Kind: v1beta1.EndpointKindUDP, TargetApp: "web", Port: 5353},
		"mdns":      {Kind: v1beta1.EndpointKindMDNS, TargetApp: "web", ServiceName: "web", Port: 5353},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if failure := Validate(context.Background(), "default", spec, declared); failure != nil {
				t.Fatalf("legal shape refused: %v", failure)
			}
		})
	}
}

func TestValidateRefusesIllegalMatrix(t *testing.T) {
	declared := facts{capable: true, safe: map[string]bool{"default/web": true}}
	cases := []struct {
		name  string
		spec  v1beta1.EndpointSpec
		field string
	}{
		{name: "missing target app", spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPHost, Host: "app.test"}, field: "spec.targetApp"},
		{name: "unknown kind", spec: v1beta1.EndpointSpec{Kind: "http.socket", TargetApp: "web"}, field: "spec.kind"},
		{name: "host shape without host", spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPHost, TargetApp: "web"}, field: "spec.host"},
		{name: "host shape with path", spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPHost, TargetApp: "web", Host: "app.test", Path: "/web"}, field: "spec.path"},
		{name: "host shape with port", spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPHost, TargetApp: "web", Host: "app.test", Port: 80}, field: "spec.port"},
		{name: "host shape with service name", spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPHost, TargetApp: "web", Host: "app.test", ServiceName: "web"}, field: "spec.serviceName"},
		{name: "path shape without path", spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPPath, TargetApp: "web", Host: "app.test"}, field: "spec.path"},
		{name: "path shape with relative path", spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPPath, TargetApp: "web", Host: "app.test", Path: "web"}, field: "spec.path"},
		{name: "path shape with port", spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPPath, TargetApp: "web", Host: "app.test", Path: "/web", Port: 80}, field: "spec.port"},
		{name: "tcp without port", spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindTCP, TargetApp: "web"}, field: "spec.port"},
		{name: "tcp with host", spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindTCP, TargetApp: "web", Port: 22, Host: "app.test"}, field: "spec.host"},
		{name: "udp with auth", spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindUDP, TargetApp: "web", Port: 53, Auth: &v1beta1.EndpointAuth{ProviderRef: "sso"}}, field: "spec.auth"},
		{name: "mdns without service name", spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindMDNS, TargetApp: "web", Port: 5353}, field: "spec.serviceName"},
		{name: "mdns with host", spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindMDNS, TargetApp: "web", ServiceName: "web", Port: 5353, Host: "app.test"}, field: "spec.host"},
		{name: "mdns with tls", spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindMDNS, TargetApp: "web", ServiceName: "web", Port: 5353, TLS: &v1beta1.EndpointTLS{}}, field: "spec.tls"},
		{name: "port out of range", spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindTCP, TargetApp: "web", Port: 70000}, field: "spec.port"},
		{name: "invalid hostname", spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPHost, TargetApp: "web", Host: "not a host"}, field: "spec.host"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			failure := Validate(context.Background(), "default", test.spec, declared)
			if failure == nil {
				t.Fatalf("illegal shape admitted: %+v", test.spec)
			}
			if failure.Field != test.field {
				t.Fatalf("field = %q, want %q (%v)", failure.Field, test.field, failure)
			}
		})
	}
}

func TestValidatePathPrefixNeedsBothDeclarations(t *testing.T) {
	cases := []struct {
		name   string
		input  Facts
		field  string
		reason string
	}{
		{name: "nothing declared", input: nil, field: "spec.kind", reason: "未声明"},
		{name: "no capability", input: facts{capable: false, safe: map[string]bool{"default/web": true}}, field: "spec.kind", reason: "network.path-prefix"},
		{name: "package does not declare", input: facts{capable: true, safe: map[string]bool{}}, field: "spec.path", reason: "未声明可安全使用路径前缀"},
		{name: "declaration unreadable", input: facts{capable: true, err: errors.New("registry down")}, field: "spec.path", reason: "registry down"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			failure := Validate(context.Background(), "default", pathSpec(), test.input)
			if failure == nil {
				t.Fatal("a path endpoint was admitted without both declarations")
			}
			if failure.Field != test.field || !strings.Contains(failure.Reason, test.reason) {
				t.Fatalf("failure = %v, want %s naming %q", failure, test.field, test.reason)
			}
		})
	}
}

func TestKeysOfSeparatesTheKeySpaces(t *testing.T) {
	cases := map[string]struct {
		spec v1beta1.EndpointSpec
		key  Key
	}{
		"host":      {spec: hostSpec(), key: Key{Space: SpaceHost, Value: "app.test"}},
		"host case": {spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPHost, TargetApp: "web", Host: "App.Test."}, key: Key{Space: SpaceHost, Value: "app.test"}},
		"path":      {spec: pathSpec(), key: Key{Space: SpacePath, Value: "app.test/web"}},
		"tcp":       {spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindTCP, TargetApp: "web", Port: 8080}, key: Key{Space: SpaceTCP, Value: "8080"}},
		"udp":       {spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindUDP, TargetApp: "web", Port: 8080}, key: Key{Space: SpaceUDP, Value: "8080"}},
		"mdns":      {spec: v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindMDNS, TargetApp: "web", ServiceName: "web", Port: 5353}, key: Key{Space: SpaceMDNS, Value: "web:5353"}},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			keys := KeysOf(test.spec)
			if len(keys) != 1 || keys[0] != test.key {
				t.Fatalf("keys = %v, want %v", keys, test.key)
			}
		})
	}
	if keys := KeysOf(v1beta1.EndpointSpec{Kind: "unknown"}); keys != nil {
		t.Fatalf("an illegal shape holds keys: %v", keys)
	}
	if SpaceTCP == SpaceUDP {
		t.Fatal("TCP and UDP share one key space")
	}
}
