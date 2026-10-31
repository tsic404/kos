// Package wm is the kos-wm first-party extension package: the window layout and
// default size preference structures (C-27) and the browser-local window set
// (C-30). A window is interaction state the browser holds; opening, minimizing
// and closing one writes no cluster object, so it never installs, uninstalls,
// scales or rebuilds the application it shows.
package wm

import (
	"encoding/json"
	"fmt"

	"github.com/tsic404/kos/pkg/extension"
)

// The package identity and the preference keys it declares.
const (
	// Identifier is the extension identity across versions (C-17).
	Identifier = "kos-wm"
	// Version is the package version this declaration belongs to.
	Version = "1.0.0"
	// PreferenceLayout is the window layout key (C-27).
	PreferenceLayout = "layout"
	// PreferenceDefaultSize is the default window size key (C-27).
	PreferenceDefaultSize = "default-size"
)

// cascadeOffset is how far each new window sits from the previous one, so a
// second instance is visible instead of hidden behind the first.
const cascadeOffset = 32

// cascadeWrap is the number of offsets a desktop reuses before starting over.
const cascadeWrap = 8

// Declaration is the kos-wm package (C-18). Window state is browser state
// (C-30), so the package contributes preference structures and nothing that
// writes the cluster.
func Declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: Identifier,
		Version:    Version,
		Contributions: extension.Contributions{
			Preferences: []extension.PreferenceSchema{{
				ID:   "windows",
				Keys: []string{PreferenceLayout, PreferenceDefaultSize},
			}},
		},
	}
}

// Size is the default window size preference (C-27): the size a new window
// takes in every browser of one account.
type Size struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// DefaultSize is the size a window takes before an account stores a preference.
var DefaultSize = Size{Width: 1280, Height: 720}

// Encode renders the size as the preference value the control plane stores.
func (s Size) Encode() (string, error) {
	return encode(s)
}

// DecodeSize reads a stored default size preference. An empty value is the
// absence of a preference, not a size of zero.
func DecodeSize(value string) (Size, error) {
	var size Size
	if err := decode(value, &size); err != nil {
		return Size{}, fmt.Errorf("decode default size: %w", err)
	}
	if size.Width <= 0 || size.Height <= 0 {
		return Size{}, fmt.Errorf("decode default size: %dx%d is not a window size", size.Width, size.Height)
	}
	return size, nil
}

// Placement is where one window sat (C-27): the window layout is the set of
// placements an account kept, so another browser of the same account lays the
// windows out the same way.
type Placement struct {
	Application string `json:"application"`
	Instance    int    `json:"instance"`
	X           int    `json:"x"`
	Y           int    `json:"y"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	IsMinimized bool   `json:"minimized,omitempty"`
}

// Layout is the window layout preference (C-27).
type Layout struct {
	Windows []Placement `json:"windows"`
}

// Encode renders the layout as the preference value the control plane stores.
func (l Layout) Encode() (string, error) {
	if l.Windows == nil {
		l.Windows = []Placement{}
	}
	return encode(l)
}

// DecodeLayout reads a stored window layout preference.
func DecodeLayout(value string) (Layout, error) {
	var layout Layout
	if err := decode(value, &layout); err != nil {
		return Layout{}, fmt.Errorf("decode layout: %w", err)
	}
	return layout, nil
}

// encode renders one preference structure as the stored string.
func encode(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode preference: %w", err)
	}
	return string(encoded), nil
}

// decode reads one stored preference structure.
func decode(value string, into any) error {
	if err := json.Unmarshal([]byte(value), into); err != nil {
		return fmt.Errorf("read preference: %w", err)
	}
	return nil
}

// Window is one open window as one browser holds it (C-30). Nothing here is a
// cluster object: a window lives and dies with the browser.
type Window struct {
	// ID identifies the window while it is open.
	ID string
	// Application is the application the window shows.
	Application string
	// Instance is the window's number among the open windows of the
	// application; opening the same application again makes another instance.
	Instance int
	// X, Y, Width and Height are the window's geometry.
	X      int
	Y      int
	Width  int
	Height int
	// IsMinimized reports a minimized window, which stays open.
	IsMinimized bool
}

// Desktop is the window set of one browser (C-30). Open, Minimize, Close and
// CloseAll change this value and nothing else: the type holds no cluster
// client, so window interaction cannot reach the application's lifecycle.
type Desktop struct {
	defaultSize Size
	windows     []Window
	opened      int
}

// New builds a browser's window set. A size without positive width and height
// falls back to DefaultSize, so a stored preference that lost its meaning still
// opens a usable window.
func New(defaultSize Size) *Desktop {
	if defaultSize.Width <= 0 || defaultSize.Height <= 0 {
		defaultSize = DefaultSize
	}
	return &Desktop{defaultSize: defaultSize}
}

// Open adds one window of the application and returns it. Opening an
// application twice gives two instances with their own windows (C-30).
func (d *Desktop) Open(application string) Window {
	d.opened++
	instance := d.instancesOf(application) + 1
	offset := cascadeOffset * ((d.opened - 1) % cascadeWrap)
	window := Window{
		ID:          fmt.Sprintf("window-%d", d.opened),
		Application: application,
		Instance:    instance,
		X:           offset,
		Y:           offset,
		Width:       d.defaultSize.Width,
		Height:      d.defaultSize.Height,
	}
	d.windows = append(d.windows, window)
	return window
}

// Minimize marks one window minimized; it stays open and keeps its place.
func (d *Desktop) Minimize(id string) bool {
	index := d.indexOf(id)
	if index < 0 {
		return false
	}
	d.windows[index].IsMinimized = true
	return true
}

// Close removes one window and reports whether it was open. The instances of
// the application that were not closed stay.
func (d *Desktop) Close(id string) bool {
	index := d.indexOf(id)
	if index < 0 {
		return false
	}
	d.windows = append(d.windows[:index], d.windows[index+1:]...)
	return true
}

// CloseAll removes every window and returns how many it closed.
func (d *Desktop) CloseAll() int {
	closed := len(d.windows)
	d.windows = nil
	return closed
}

// Windows returns the open windows in the order they were opened.
func (d *Desktop) Windows() []Window {
	return append([]Window(nil), d.windows...)
}

// Count returns how many windows are open.
func (d *Desktop) Count() int { return len(d.windows) }

// DefaultSize returns the size new windows take in this browser.
func (d *Desktop) DefaultSize() Size { return d.defaultSize }

// Layout is the window layout the account keeps (C-27), so another browser of
// the same account restores the same windows.
func (d *Desktop) Layout() Layout {
	layout := Layout{Windows: make([]Placement, 0, len(d.windows))}
	for _, window := range d.windows {
		layout.Windows = append(layout.Windows, Placement{
			Application: window.Application,
			Instance:    window.Instance,
			X:           window.X,
			Y:           window.Y,
			Width:       window.Width,
			Height:      window.Height,
			IsMinimized: window.IsMinimized,
		})
	}
	return layout
}

// instancesOf counts the open windows of one application.
func (d *Desktop) instancesOf(application string) int {
	count := 0
	for _, window := range d.windows {
		if window.Application == application {
			count++
		}
	}
	return count
}

// indexOf finds one open window.
func (d *Desktop) indexOf(id string) int {
	for index, window := range d.windows {
		if window.ID == id {
			return index
		}
	}
	return -1
}
