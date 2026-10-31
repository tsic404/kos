// Package kosnetwork is the kos-network product: the network view of the panel.
// It declares the network page of C-50 and reads endpoints with their
// certificates, the port ledger projection of C-44, a read-time DNS probe and
// the certificate expiry alert of C-46.
package kosnetwork

import "github.com/tsic404/kos/pkg/extension"

// Identifier is the product's extension identity across versions (C-17).
const Identifier = "kos-network"

// PageNetwork is the network page (C-50).
const PageNetwork = "network"

// Item identities of the network page, one per source C-50 names.
const (
	ItemEndpoints    = "endpoints-and-certificates"
	ItemLedger       = "port-ledger"
	ItemResolution   = "domain-resolution"
	ItemCertificates = "certificate-expiry"
)

// ReadOnlyReason explains why the page offers no write entry: endpoints belong
// to the application that declares them (C-43, C-45).
const ReadOnlyReason = "本页只读：入口随应用声明建立并由入口对象调和，网络页不提供修改入口"

// Declaration returns the contribution kos-network ships (C-18).
func Declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: Identifier,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Pages: []extension.Page{networkPage()},
		},
	}
}

// networkPage declares the network items C-50 names.
func networkPage() extension.Page {
	return extension.Page{
		ID:             PageNetwork,
		Title:          "网络",
		Location:       "panel",
		ReadOnly:       true,
		ReadOnlyReason: ReadOnlyReason,
		Items: []extension.PageItem{
			{ID: ItemEndpoints, Label: "入口与证书状态", Source: extension.PageSourceObject, Reference: reference(ItemEndpoints)},
			{ID: ItemLedger, Label: "端口台账", Source: extension.PageSourceObject, Reference: reference(ItemLedger)},
			{ID: ItemResolution, Label: "域名解析", Source: extension.PageSourceProbe, Reference: reference(ItemResolution)},
			{ID: ItemCertificates, Label: "证书到期告警", Source: extension.PageSourceComputed, Reference: reference(ItemCertificates)},
		},
	}
}

// reference is the item reference the product owns; the router dispatches by
// the identifier in front of it (C-22).
func reference(item string) string { return Identifier + "/" + item }
