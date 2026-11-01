package harness

import (
	"os"
	"testing"

	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/tsic404/kos/pkg/install"
)

// InstallerIdentity is the API server user the installation runs as.
func InstallerIdentity() string {
	return ServiceAccountUser(install.DefaultNamespace, install.InstallerServiceAccount)
}

// ControlPlaneIdentity is the API server user the control-plane reconcilers
// run as; their writes are KOS writes too (C-15).
func ControlPlaneIdentity() string {
	return ServiceAccountUser(install.DefaultNamespace, install.ControlPlaneServiceAccount)
}

// Definitions reads the release's object definition manifests.
func Definitions(t *testing.T) install.DefinitionSource {
	t.Helper()
	return install.CRDDirectory{FS: os.DirFS(RepoRoot(t)), Dir: "config/crd"}
}

// InstallClients are the clients one installation run uses, acting as the
// installer identity.
type InstallClients struct {
	Dynamic       dynamic.Interface
	Kube          kubernetes.Interface
	APIExtensions apiextensionsclientset.Interface
}

// InstallClients builds the installation clients for one identity.
func (c *Cluster) InstallClients(t *testing.T, username string) InstallClients {
	t.Helper()
	return InstallClients{
		Dynamic:       c.DynamicAs(t, username),
		Kube:          c.KubeAs(t, username),
		APIExtensions: c.ExtensionsAs(t, username),
	}
}

// AuditSource builds the audit reader of an audited cluster.
func (c *Cluster) AuditSource(t *testing.T) install.AuditSource {
	t.Helper()
	if c.AuditLogPath == "" {
		return &install.AuditLog{}
	}
	kinds, err := install.DiscoveryKinds(c.Kube.Discovery())
	if err != nil {
		t.Fatalf("resolve audit resources: %v", err)
	}
	return &install.AuditLog{Path: c.AuditLogPath, Kinds: kinds}
}

// InstallerPlan is an existing-cluster plan with the acceptance defaults.
func InstallerPlan(overrides func(*install.Plan)) install.Plan {
	plan := install.Plan{
		Mode:         install.ModeExisting,
		Owner:        "owner",
		Applications: []string{Namespace},
	}
	if overrides != nil {
		overrides(&plan)
	}
	return plan
}

// BootstrapPlan is a bootstrap plan with the acceptance defaults.
func BootstrapPlan(nodes int, overrides func(*install.Plan)) install.Plan {
	plan := install.Plan{
		Mode:         install.ModeBootstrap,
		Owner:        "owner",
		Nodes:        nodes,
		Applications: []string{Namespace},
	}
	if overrides != nil {
		overrides(&plan)
	}
	return plan
}

// Endpoint is this cluster as a control plane in another cluster reaches it
// (C-54 external form).
func (c *Cluster) Endpoint() install.ManagedEndpoint {
	return install.ManagedEndpoint{Server: c.Config.Host, Authority: c.Config.CAData}
}

// ExternalPlan is an external-control-plane plan that reconciles this cluster
// while the control plane runs in another one (C-54).
func (c *Cluster) ExternalPlan(overrides func(*install.Plan)) install.Plan {
	plan := install.Plan{
		Mode:         install.ModeExternal,
		Owner:        "owner",
		Applications: []string{Namespace},
		Managed:      c.Endpoint(),
	}
	if overrides != nil {
		overrides(&plan)
	}
	return plan
}
