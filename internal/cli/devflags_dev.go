//go:build dev

package cli

import "github.com/spf13/cobra"

// addDevFlags adds the two flags the CI end-to-end jobs need to run the branch build of the images (ruling S9).
// They exist only in binaries built with `go build -tags dev`; the release build (GoReleaser, no tag) does not
// have them, so a user cannot pass them.
func addDevFlags(up *cobra.Command, f *localFlags) {
	up.Flags().StringArrayVar(&f.devArchives, "dev-load-archive", nil, "development builds only: load this image archive (docker save output) into the kind node")
	up.Flags().StringArrayVar(&f.devValues, "dev-values", nil, "development builds only: values file merged after the quick-start values")
}

// addKeyringFlag adds --dev-keyring-service, so end-to-end runs and developers never touch the keychain item of
// the real ambit. Release builds do not have it.
func addKeyringFlag(c *cobra.Command, service *string) {
	c.Flags().StringVar(service, "dev-keyring-service", "", "development builds only: keychain service name to use instead of \"ambit\"")
}

// addChartDirFlag adds --chart-dir to install and upgrade: a development build has no chart version, so it
// installs the charts of a checkout (spec D2 3.2). Release builds always install the charts of their own version.
func addChartDirFlag(c *cobra.Command, dir *string) {
	c.Flags().StringVar(dir, "chart-dir", "", "development builds only: install the charts from this directory (charts/ of a checkout)")
}
