//go:build !windows

package out

import "io"

// enableConsoleVT is only needed on the Windows console; every other terminal reads colour codes.
func enableConsoleVT(io.Writer) bool { return true }
