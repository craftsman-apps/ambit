package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// lintObject is what lint reads of one YAML document: an AccessRule, or a List of objects.
type lintObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec  v1.AccessRuleSpec `json:"spec"`
	Items []json.RawMessage `json:"items"`
}

// lintRule returns the warnings for one AccessRule that the cluster accepts but that gives more access than people
// usually mean. Each ends with its ID in brackets.
func lintRule(spec v1.AccessRuleSpec) []string {
	var w []string
	if spec.AllSignedIn {
		w = append(w, "matches all signed-in users; name groups or users [all-signed-in]")
	}
	for _, s := range spec.Servers {
		if len(s.Tools) == 0 {
			w = append(w, "allows every tool of server "+s.Name+", including tools added later; list the tools [all-tools]")
		}
	}
	return w
}

// lintFile returns the warnings of the AccessRules in r, as "AccessRule <name>: <warning>", and how many it read.
func lintFile(r io.Reader) (warnings []string, rules int, err error) {
	dec := utilyaml.NewYAMLReader(bufio.NewReader(r))
	var visit func(o lintObject) error
	visit = func(o lintObject) error {
		if o.Kind == "List" {
			for i, it := range o.Items {
				var item lintObject
				if err := json.Unmarshal(it, &item); err != nil {
					return fmt.Errorf("item %d of a List is not an object: %w", i, err)
				}
				if err := visit(item); err != nil {
					return err
				}
			}
			return nil
		}
		if o.Kind != "AccessRule" || !strings.HasPrefix(o.APIVersion, "ambitproject.dev/") {
			return nil
		}
		rules++
		for _, w := range lintRule(o.Spec) {
			warnings = append(warnings, "AccessRule "+o.Metadata.Name+": "+w)
		}
		return nil
	}
	for {
		doc, err := dec.Read()
		if errors.Is(err, io.EOF) {
			return warnings, rules, nil
		}
		if err != nil {
			return nil, 0, err
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		var o lintObject
		if err := yaml.Unmarshal(doc, &o); err != nil {
			return nil, 0, fmt.Errorf("not valid YAML: %w", err)
		}
		if err := visit(o); err != nil {
			return nil, 0, err
		}
	}
}

// ghEscape escapes a GitHub workflow command value; prop also escapes the separators of a property.
func ghEscape(s string, prop bool) string {
	r := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")
	s = r.Replace(s)
	if prop {
		s = strings.NewReplacer(",", "%2C", ":", "%3A").Replace(s)
	}
	return s
}

func (a *app) lintCmd() *cobra.Command {
	var strict bool
	c := &cobra.Command{
		Use:   "lint <file or directory>...",
		Short: "Warn on AccessRules that the cluster accepts but that give wide access",
		Long: `Reads the YAML files (also a List, as kubectl get accessrules -o yaml prints it) and prints one warning for each
risky setting of an AccessRule:

  all-signed-in  allSignedIn: true matches every signed-in user
  all-tools      a server without a tools list allows every tool, including tools the server adds later

It reads files only, no cluster. Exits 0 with warnings; with --strict it exits 1 when there is a warning.`,
		Args: cobra.MinimumNArgs(1),
		RunE: run(func(cmd *cobra.Command, paths []string) error {
			p := a.printer()
			github := os.Getenv("GITHUB_ACTIONS") == "true"
			files, rules, total := 0, 0, 0
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
					warnings, n, err := lintFile(f)
					if err != nil {
						return &out.Error{What: path, Why: err.Error(), Code: out.ExitUsage}
					}
					files, rules, total = files+1, rules+n, total+len(warnings)
					for _, w := range warnings {
						p.Line("%s %s: %s", p.Mark("warn"), path, w)
						if github {
							p.Line("::warning file=%s::%s", ghEscape(path, true), ghEscape(w, false))
						}
					}
					return nil
				})
				if err != nil {
					var e *out.Error
					if errors.As(err, &e) {
						return e
					}
					return &out.Error{What: root, Why: err.Error(), Next: "check the path", Code: out.ExitUsage}
				}
			}
			p.Line("%d files, %d AccessRules, %d warnings", files, rules, total)
			if strict && total > 0 {
				return exitCode(out.ExitFailed)
			}
			return nil
		}),
	}
	c.Flags().BoolVar(&strict, "strict", false, "exit 1 when there is a warning")
	return c
}
