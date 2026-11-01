// Package degrade carries the process-wide degradation runtime of C-56 and
// C-57: the read-only state the panel shows as a banner, and the memory
// protection that turns native-view real-time listening off when the control
// plane exceeds its budget. The behaviours of C-56 belong to the components that
// own them; this package only fixes the shared state and the protection
// hysteresis, so no component invents its own.
package degrade
