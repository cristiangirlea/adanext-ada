// Package ada is Ada's core. See docs/design.md for the conversation loop and provider design.
package ada

import "runtime/debug"

// Version reports the module version embedded in the Ada executable.
func Version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
