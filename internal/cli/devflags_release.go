//go:build !dev

package cli

import "github.com/spf13/cobra"

// addDevFlags adds nothing in a release build. See devflags_dev.go.
func addDevFlags(*cobra.Command, *localFlags) {}

// addKeyringFlag adds nothing in a release build. See devflags_dev.go.
func addKeyringFlag(*cobra.Command, *string) {}
