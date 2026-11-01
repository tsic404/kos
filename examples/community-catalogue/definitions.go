package communitycatalogue

import (
	"fmt"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/extension"
)

// offer is one definition this source offers: the identity the catalogue entry
// names, the display information the page shows, and the definition the sync
// writes (C-24).
type offer struct {
	// ID is the entry identity and the definition name it installs.
	ID string
	// Name is the display name.
	Name string
	// Description explains what the application does.
	Description string
	// Icon is the display icon key.
	Icon string
	// Spec is the definition the entry installs (C-32).
	Spec v1beta1.ApplicationDefinitionSpec
}

// template renders one workload and the service in front of it. The template is
// the whole application: the definition carries no volume, endpoint or storage
// class, so installing it stays inside the declared namespace (C-32).
const template = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  replicas: {{ .Config.replicas }}
  selector:
    matchLabels:
      app: {{ .Name }}
  template:
    metadata:
      labels:
        app: {{ .Name }}
    spec:
      containers:
        - name: {{ .Name }}
          image: {{ .Config.image }}
          ports:
            - containerPort: {{ .Config.httpPort }}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  selector:
    app: {{ .Name }}
  ports:
    - port: {{ .Config.httpPort }}
      targetPort: {{ .Config.httpPort }}
`

// offers are the applications this source ships. They are the source's own
// catalogue: a third party publishes its entries here, not in the kernel.
var offers = []struct {
	id          string
	name        string
	description string
	icon        string
	image       string
	port        string
}{
	{
		id: "community-wiki", name: "Community Wiki",
		description: "社区维护的文档站点", icon: "wiki",
		image: "ghcr.io/example/wiki:1.4", port: "8080",
	},
	{
		id: "community-status", name: "Community Status",
		description: "社区状态页", icon: "status",
		image: "ghcr.io/example/status:2.1", port: "3000",
	},
}

// catalogueSource builds the source the package contributes, with one entry per
// shipped definition (C-24).
func catalogueSource() (extension.CatalogueSource, error) {
	entries := make([]extension.CatalogueEntry, 0, len(offers))
	for _, item := range offers {
		spec, err := definitionOf(item.image, item.port)
		if err != nil {
			return extension.CatalogueSource{}, fmt.Errorf("build the %s definition: %w", item.id, err)
		}
		entries = append(entries, extension.CatalogueEntry{
			ID:         item.id,
			Display:    extension.CatalogueDisplay{Name: item.name, Description: item.description, Icon: item.icon},
			Definition: spec,
		})
	}
	return extension.CatalogueSource{ID: SourceID, Sync: SyncPull, Entries: entries}, nil
}

// definitionOf builds one definition and pins the digest of its content, the way
// C-32 requires: the catalogue installs an entry only when the package matches
// the digest it declares.
func definitionOf(image, port string) (v1beta1.ApplicationDefinitionSpec, error) {
	spec := v1beta1.ApplicationDefinitionSpec{
		Package: v1beta1.PackageIdentity{Version: "1.0.0"},
		Config: []v1beta1.ConfigField{
			{Name: "replicas", Type: "integer", Default: pointer("1"), Minimum: pointer("1"),
				Maximum: pointer("3"), Description: "Number of replicas."},
			{Name: "image", Type: "string", Default: pointer(image), Description: "Container image to run."},
			{Name: "httpPort", Type: "string", Default: pointer(port), Description: "Port the service exposes."},
		},
		Template: template,
	}
	digest, err := app.DefinitionDigest(spec)
	if err != nil {
		return v1beta1.ApplicationDefinitionSpec{}, err
	}
	spec.Package.Digest = digest
	if err := app.ValidateDefinition(spec); err != nil {
		return v1beta1.ApplicationDefinitionSpec{}, err
	}
	return spec, nil
}

// pointer is the textual default of one config field.
func pointer(value string) *string { return &value }

// offered lists the definition names this source offers, which is what the
// upgrade read filters on.
func offered() map[string]bool {
	names := make(map[string]bool, len(offers))
	for _, item := range offers {
		names[item.id] = true
	}
	return names
}
