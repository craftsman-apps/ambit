package cli

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/craftsman-apps/ambit/internal/cli/out"
	"github.com/craftsman-apps/ambit/internal/validate"
)

func (a *app) validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <file or directory>...",
		Short: "Check Ambit objects in YAML files the way the cluster would",
		Long: `Checks every Ambit object (apiVersion ambitproject.dev/...) in the YAML files: defaults, unknown
fields, the schema and the CEL rules, exactly as the cluster would on apply. Other objects are skipped.
Exits 1 if any object would be refused.`,
		Args: cobra.MinimumNArgs(1),
		RunE: run(func(cmd *cobra.Command, paths []string) error {
			p := a.printer()
			v, err := validate.New()
			if err != nil {
				return &out.Error{What: "validate", Why: err.Error(), Code: out.ExitUsage}
			}
			github := os.Getenv("GITHUB_ACTIONS") == "true"
			files, objects, bad := 0, 0, 0
			for _, root := range paths {
				err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
					if err != nil || d.IsDir() || (!strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml")) {
						return err
					}
					f, err := os.Open(path)
					if err != nil {
						return err
					}
					defer f.Close()
					problems, n, err := v.File(context.Background(), f)
					if err != nil {
						return &out.Error{What: path, Why: err.Error(), Code: out.ExitUsage}
					}
					files, objects, bad = files+1, objects+n, bad+len(problems)
					for _, pr := range problems {
						p.Line("%s: %s", path, pr)
						if github {
							p.Line("::error file=%s::%s", path, pr)
						}
					}
					return nil
				})
				if err != nil {
					if e, ok := err.(*out.Error); ok {
						return e
					}
					return &out.Error{What: root, Why: err.Error(), Next: "check the path", Code: out.ExitUsage}
				}
			}
			p.Line("%d Ambit objects in %d files: %d problems", objects, files, bad)
			if bad > 0 {
				return exitCode(out.ExitFailed)
			}
			return nil
		}),
	}
}
