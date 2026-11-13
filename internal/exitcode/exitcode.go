// Package exitcode defines the process exit codes. Only main maps errors
// to these values; nothing else calls os.Exit.
package exitcode

const (
	// Green means every host is GREEN.
	Green = 0
	// Yellow means the worst host is YELLOW.
	Yellow = 1
	// Red means the worst host is RED.
	Red = 2
	// Config is a configuration or usage error.
	Config = 3
	// Runtime is a runtime or I/O error.
	Runtime = 4
	// Blind means the vantage could not see the mesh.
	Blind = 5
)

// WritesDocument reports whether a run that ends with this code writes the
// probe document (stdout under --json, and the --out file). Codes 3 and 4
// never write one.
func WritesDocument(code int) bool {
	switch code {
	case Green, Yellow, Red, Blind:
		return true
	default:
		return false
	}
}
