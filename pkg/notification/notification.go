// Package notification is the kos-notification first-party extension package:
// it reads the recent events of C-64 and delivers them to external channels,
// recording each attempt as an object of its own class.
//
// It contributes exactly what C-18 allows: a delivery record class, a
// controller in the extension's own API group, and the channel and subscription
// setting items. The in-app notification center is the desktop page another
// product declares (C-52); channel credentials live in the control plane and
// never enter a cluster object, a log or the declaration (C-14).
package notification

import (
	"github.com/tsic404/kos/pkg/extension"
)

// The package identity and the surface its declaration names (C-17, C-18).
const (
	// Identifier is the extension identity across versions.
	Identifier = "kos-notification"
	// Version is the package version this declaration belongs to.
	Version = "1.0.0"
	// APIGroup is the extension's own API group; no class touches the kernel
	// group (C-19).
	APIGroup = "notification.kos.tsic.top"
	// ObjectVersion is the version of the delivery record class.
	ObjectVersion = "v1"
	// Kind is the delivery record class.
	Kind = "DeliveryRecord"
	// Resource is the class's lowercase plural name.
	Resource = "deliveryrecords"
	// ControllerID names the delivery controller contribution.
	ControllerID = "delivery"
)

// The setting items the product contributes (C-23). Every item is cluster
// scoped and written by the user through the control plane; the controller
// only reads them.
const (
	// SettingChannels lists the external channels a delivery may target. It
	// carries the channel shape, never the credential (C-14); the control plane
	// registers each channel's secret together with its endpoint, so an endpoint
	// rewritten here does not receive that secret.
	SettingChannels = "notification.channels"
	// SettingSubscriptions lists which events each channel receives.
	SettingSubscriptions = "notification.subscriptions"
)

// The record's own status vocabulary. The record is an extension object, so
// its spec and status semantics are the product's (C-19); Reconciled and the
// failure condition follow C-07.
const (
	// PhaseDelivered is a record whose message the external channel accepted.
	PhaseDelivered = "Delivered"
	// PhaseFailed is a record the channel refused in its last attempt.
	PhaseFailed = "Failed"
	// ConditionDeliveryFailed is the record's failure condition (C-07).
	ConditionDeliveryFailed = "DeliveryFailed"
)

// Declaration returns the package declaration (C-18): one object class, one
// controller, two setting items and the declared read of events.
func Declaration() extension.Declaration {
	return extension.Declaration{
		Identifier:       Identifier,
		Version:          Version,
		FrameworkVersion: ">=1.0 <2.0",
		APIGroup:         APIGroup,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group:      APIGroup,
				Kind:       Kind,
				Resource:   Resource,
				Versions:   []string{ObjectVersion},
				Namespaced: false,
			}},
			Controllers: []extension.Controller{{
				ID:      ControllerID,
				Manages: []extension.GroupKind{{Group: APIGroup, Kind: Kind}},
				Scope:   extension.WriteScope{Groups: []string{APIGroup}},
			}},
			Settings: []extension.Setting{
				{ID: SettingChannels, Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterUser, Editable: true},
				{ID: SettingSubscriptions, Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterUser, Editable: true},
			},
		},
		Permissions: []extension.Permission{{
			Contribution: ControllerID,
			ReadScope:    extension.ReadScope{Kinds: []extension.GroupKind{{Kind: "Event"}}},
		}},
		Configuration: extension.Configuration{
			Schema:     settingsSchema(),
			Conditions: []string{ConditionDeliveryFailed},
		},
		Retention: []extension.Retention{{Resource: Resource, Keep: false}},
	}
}

// settingsSchema describes the two setting values, so a reader can render the
// channel and subscription forms without knowing the package internals (C-18).
func settingsSchema() map[string]any {
	return map[string]any{
		"channels": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"channels": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type":     "object",
						"required": []any{"id", "kind", "endpoint"},
						"properties": map[string]any{
							"id":       map[string]any{"type": "string"},
							"kind":     map[string]any{"type": "string", "enum": []any{"webhook"}},
							"endpoint": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
		"subscriptions": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"subscriptions": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type":     "object",
						"required": []any{"id", "channels"},
						"properties": map[string]any{
							"id":       map[string]any{"type": "string"},
							"types":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"reasons":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"channels": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						},
					},
				},
			},
		},
	}
}
