// ambit is Ambit's command-line tool. `ambit validate` checks Ambit objects in a configuration
// repository the way the Kubernetes API server would, so a bad change fails in the pull request.
package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/craftsman-apps/ambit/internal/validate"
)

const usage = `usage: ambit validate <file or directory>...

Checks every Ambit object (apiVersion ambitproject.dev/...) in the YAML files: defaults, unknown
fields, the schema and the CEL rules, exactly as the cluster would on apply. Other objects are skipped.
Exits 1 if any object would be refused.`

func main() {
	if len(os.Args) < 3 || os.Args[1] != "validate" {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	os.Exit(run(os.Args[2:]))
}

func run(paths []string) int {
	v, err := validate.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ambit:", err)
		return 2
	}
	github := os.Getenv("GITHUB_ACTIONS") == "true"
	files, objects, bad := 0, 0, 0
	for _, root := range paths {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || (!strings.HasSuffix(p, ".yaml") && !strings.HasSuffix(p, ".yml")) {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			problems, n, err := v.File(context.Background(), f)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			files, objects, bad = files+1, objects+n, bad+len(problems)
			for _, pr := range problems {
				fmt.Printf("%s: %s\n", p, pr)
				if github {
					fmt.Printf("::error file=%s::%s\n", p, pr)
				}
			}
			return nil
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "ambit:", err)
			return 2
		}
	}
	fmt.Printf("%d Ambit objects in %d files: %d problems\n", objects, files, bad)
	if bad > 0 {
		return 1
	}
	return 0
}
