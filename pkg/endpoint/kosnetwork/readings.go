package kosnetwork

import (
	"context"
	"net"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/endpoint"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/pages/cluster"
)

// probeTimeout bounds one DNS probe: the page reads a result with its
// observation time, never a blocked request (C-50).
const probeTimeout = 2 * time.Second

// probeLimit bounds how many hosts one read probes.
const probeLimit = 32

// Resolver is the read-time name resolver the domain probe uses; net.Resolver
// implements it, and a deployment may supply another one.
type Resolver interface {
	// LookupHost resolves a host name to its addresses.
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// Readings resolves the items the network page declares (C-53).
type Readings struct {
	// Dynamic reads the cluster.
	Dynamic dynamic.Interface
	// Now supplies the read clock; defaults to time.Now.
	Now func() time.Time
	// Resolver answers the read-time probe; nil uses the process resolver.
	Resolver Resolver
}

// Read implements pages.Readings.
func (r *Readings) Read(ctx context.Context, session identity.Session, item extension.PageItem) (pages.Reading, *controlplane.Error) {
	value, failure := r.value(ctx, item)
	if failure != nil {
		return pages.Reading{}, failure
	}
	return pages.Reading{Value: value, ObservedAt: r.reader().Clock()}, nil
}

// value resolves one item from the source it declares.
func (r *Readings) value(ctx context.Context, item extension.PageItem) (any, *controlplane.Error) {
	switch item.ID {
	case ItemEndpoints:
		return r.endpoints(ctx)
	case ItemLedger:
		return r.ledger(ctx)
	case ItemResolution:
		return r.resolution(ctx)
	case ItemCertificates:
		return r.certificates(ctx)
	default:
		return nil, unknownItem(item.ID)
	}
}

// reader is the cluster read seam for this call.
func (r *Readings) reader() *cluster.Reader {
	return &cluster.Reader{Dynamic: r.Dynamic, Now: r.Now}
}

// certificateView is the observed certificate state of one endpoint.
type certificateView struct {
	Issuer       string `json:"issuer"`
	Ready        bool   `json:"ready"`
	NotAfter     string `json:"notAfter,omitempty"`
	ExpiringSoon bool   `json:"expiringSoon"`
	Reason       string `json:"reason,omitempty"`
}

// endpointView is one endpoint as the page shows it (C-43, C-46).
type endpointView struct {
	Namespace    string           `json:"namespace"`
	Name         string           `json:"name"`
	Kind         string           `json:"kind"`
	TargetApp    string           `json:"targetApp"`
	Host         string           `json:"host,omitempty"`
	Path         string           `json:"path,omitempty"`
	Port         int32            `json:"port,omitempty"`
	ServiceName  string           `json:"serviceName,omitempty"`
	IssuerRef    string           `json:"issuerRef,omitempty"`
	AuthProvider string           `json:"authProviderRef,omitempty"`
	Phase        string           `json:"phase,omitempty"`
	Health       string           `json:"health,omitempty"`
	Certificate  *certificateView `json:"certificate,omitempty"`
}

// endpoints reads the endpoints with their certificate state (C-50).
func (r *Readings) endpoints(ctx context.Context) (any, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "Endpoint", "")
	if failure != nil {
		return nil, failure
	}
	views := make([]endpointView, 0, len(objects))
	for _, object := range objects {
		var stored v1beta1.Endpoint
		if failure := cluster.Decode(object, &stored); failure != nil {
			return nil, failure
		}
		views = append(views, endpointOf(stored))
	}
	sort.Slice(views, func(left, right int) bool {
		if views[left].Namespace != views[right].Namespace {
			return views[left].Namespace < views[right].Namespace
		}
		return views[left].Name < views[right].Name
	})
	return map[string]any{"endpoints": views}, nil
}

// endpointOf renders one endpoint.
func endpointOf(stored v1beta1.Endpoint) endpointView {
	view := endpointView{
		Namespace: stored.Namespace, Name: stored.Name,
		Kind: string(stored.Spec.Kind), TargetApp: stored.Spec.TargetApp,
		Host: stored.Spec.Host, Path: stored.Spec.Path, Port: stored.Spec.Port,
		ServiceName: stored.Spec.ServiceName,
		Phase:       stored.Status.Phase, Health: string(stored.Status.Health.State),
	}
	if stored.Spec.TLS != nil {
		view.IssuerRef = stored.Spec.TLS.IssuerRef
	}
	if stored.Spec.Auth != nil {
		view.AuthProvider = stored.Spec.Auth.ProviderRef
	}
	if certificate := stored.Status.Certificate; certificate != nil {
		view.Certificate = &certificateView{
			Issuer: certificate.Issuer, Ready: certificate.Ready,
			NotAfter: timeText(certificate.NotAfter), ExpiringSoon: certificate.ExpiringSoon,
			Reason: certificate.Reason,
		}
	}
	return view
}

// timeText renders an optional timestamp.
func timeText(moment *metav1.Time) string {
	if moment == nil {
		return ""
	}
	return moment.UTC().Format(time.RFC3339)
}

// ledger rebuilds the C-44 allocation projection from the stored endpoints and
// returns the rows with their keys, conflicts and certificate alert.
func (r *Readings) ledger(ctx context.Context) (any, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "Endpoint", "")
	if failure != nil {
		return nil, failure
	}
	applications, failure := r.applicationStates(ctx)
	if failure != nil {
		return nil, failure
	}
	entries := make([]endpoint.Entry, 0, len(objects))
	for _, object := range objects {
		unstructuredObject := &unstructured.Unstructured{Object: object}
		entry := endpoint.EntryOf(unstructuredObject)
		var stored v1beta1.Endpoint
		if failure := cluster.Decode(object, &stored); failure != nil {
			return nil, failure
		}
		entry.Certificate = certificateOf(stored.Status.Certificate)
		target := entry.Ref.Namespace + "/" + stored.Spec.TargetApp
		if application, known := applications[target]; known {
			entry.TargetPaused = application.paused
		} else {
			entry.TargetMissing = true
		}
		entries = append(entries, entry)
	}
	ledger := endpoint.NewLedger()
	ledger.Replace(entries)
	return map[string]any{
		"projection": ledger.ProjectionAt(r.reader().Clock()),
		"note":       endpoint.SharedMediumNote,
	}, nil
}

// applicationState is what the ledger needs to judge an endpoint's target.
type applicationState struct {
	paused bool
}

// applicationStates indexes the applications an endpoint may target.
func (r *Readings) applicationStates(ctx context.Context) (map[string]applicationState, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "Application", "")
	if failure != nil {
		return nil, failure
	}
	applications := make(map[string]applicationState, len(objects))
	for _, object := range objects {
		var application v1beta1.Application
		if failure := cluster.Decode(object, &application); failure != nil {
			return nil, failure
		}
		applications[application.Namespace+"/"+application.Name] = applicationState{paused: application.Spec.Paused}
	}
	return applications, nil
}

// certificateOf converts the stored certificate state into the ledger's own
// shape (C-46).
func certificateOf(stored *v1beta1.EndpointCertificate) *endpoint.Certificate {
	if stored == nil {
		return nil
	}
	certificate := &endpoint.Certificate{
		Issuer: stored.Issuer, Ready: stored.Ready,
		ExpiringSoon: stored.ExpiringSoon, Reason: stored.Reason,
	}
	if stored.NotAfter != nil {
		moment := stored.NotAfter.UTC()
		certificate.NotAfter = &moment
	}
	return certificate
}

// resolutionResult is one host's read-time probe result.
type resolutionResult struct {
	Host      string   `json:"host"`
	Addresses []string `json:"addresses,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// resolution probes the hosts the endpoints declare (C-50, C-53).
func (r *Readings) resolution(ctx context.Context) (any, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "Endpoint", "")
	if failure != nil {
		return nil, failure
	}
	hosts := make([]string, 0, len(objects))
	seen := map[string]bool{}
	for _, object := range objects {
		var stored v1beta1.Endpoint
		if failure := cluster.Decode(object, &stored); failure != nil {
			return nil, failure
		}
		host := stored.Spec.Host
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	if len(hosts) > probeLimit {
		hosts = hosts[:probeLimit]
	}
	resolver := r.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	// One deadline bounds the whole probe, so a slow resolver cannot hold the
	// page read beyond C-13's freshness bound.
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	results := make([]resolutionResult, 0, len(hosts))
	for _, host := range hosts {
		addresses, err := resolver.LookupHost(probeCtx, host)
		result := resolutionResult{Host: host, Addresses: addresses}
		if err != nil {
			result.Addresses = nil
			result.Error = err.Error()
		}
		results = append(results, result)
	}
	return map[string]any{"probe": "dns", "timeout": probeTimeout.String(), "results": results}, nil
}

// certificateAlert is one endpoint whose certificate needs attention (C-46).
type certificateAlert struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Issuer       string `json:"issuer"`
	NotAfter     string `json:"notAfter,omitempty"`
	ExpiringSoon bool   `json:"expiringSoon"`
	Reason       string `json:"reason,omitempty"`
}

// certificates computes the expiry alert C-50 names: a certificate that is not
// ready, reports an imminent expiry, or expires inside C-46's warning window.
func (r *Readings) certificates(ctx context.Context) (any, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "Endpoint", "")
	if failure != nil {
		return nil, failure
	}
	now := r.reader().Clock()
	alerts := make([]certificateAlert, 0)
	for _, object := range objects {
		var stored v1beta1.Endpoint
		if failure := cluster.Decode(object, &stored); failure != nil {
			return nil, failure
		}
		certificate := stored.Status.Certificate
		if certificate == nil {
			continue
		}
		imminent := certificate.NotAfter != nil && now.Add(endpoint.CertificateWarningWindow).After(certificate.NotAfter.Time)
		if certificate.Ready && !certificate.ExpiringSoon && !imminent {
			continue
		}
		alerts = append(alerts, certificateAlert{
			Namespace: stored.Namespace, Name: stored.Name, Issuer: certificate.Issuer,
			NotAfter: timeText(certificate.NotAfter), ExpiringSoon: certificate.ExpiringSoon || imminent,
			Reason: certificate.Reason,
		})
	}
	sort.Slice(alerts, func(left, right int) bool {
		if alerts[left].Namespace != alerts[right].Namespace {
			return alerts[left].Namespace < alerts[right].Namespace
		}
		return alerts[left].Name < alerts[right].Name
	})
	return map[string]any{
		"window": endpoint.CertificateWarningWindow.String(),
		"alerts": alerts,
	}, nil
}

// unknownItem marks an item this product does not declare.
func unknownItem(id string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeCapabilityMissing,
		Message:     "kos-network 没有声明页面条目 " + id,
		Stage:       controlplane.StageCapability,
		SideEffects: controlplane.SideEffectsNone,
	}
}
