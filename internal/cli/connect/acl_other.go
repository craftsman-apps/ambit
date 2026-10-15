//go:build !windows

package connect

// restrictToUser does nothing: the 0600 mode is the whole protection outside Windows.
func restrictToUser(string) error { return nil }

func isWindowsChmodError(error) bool { return false }
