package checks

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/craftsman-apps/ambit/internal/cli/out"
)

func TestRunTurnsAPanicIntoUnknown(t *testing.T) {
	old := Catalogue
	t.Cleanup(func() { Catalogue = old })
	Catalogue = []Check{{"X1", GroupPre, func(context.Context, *Env) Result { var m map[string]int; m["a"]++; return Result{} }}}
	rs := Run(context.Background(), fx{}.env(t), map[string]bool{GroupPre: true})
	if len(rs) != 1 || rs[0].ID != "X1" || rs[0].Status != Unknown || !strings.Contains(rs[0].Message, "internal error") {
		t.Fatalf("rs = %+v", rs)
	}
}

func TestSummaryAndExitCode(t *testing.T) {
	rs := []Result{{Status: Pass}, {Status: Pass}, {Status: Warn}, {Status: Fail}, {Status: Unknown}, {Status: NA}}
	if s := Summary(rs); s != "2 passed, 1 warning, 1 failed, 1 unknown, 1 n/a" {
		t.Fatalf("summary %q", s)
	}
	if s := Summary(rs[:3]); s != "2 passed, 1 warning, 0 failed" {
		t.Fatalf("summary %q", s)
	}
	if ExitCode(rs, false) != 1 || ExitCode(rs[:3], false) != 0 {
		t.Fatal("exit code: a failed check must give 1, warnings 0")
	}
	if ExitCode([]Result{{Status: Unknown}}, false) != 0 || ExitCode([]Result{{Status: Unknown}}, true) != 1 {
		t.Fatal("--fail-on-unknown")
	}
}

func TestPrint(t *testing.T) {
	var b bytes.Buffer
	p := out.New(out.Options{Out: &b, Err: &b}, &out.Redactor{})
	Print(p, []Result{
		{ID: "P1", Status: Fail, Message: "gateway.replicas is 1; production needs 2 or more", Next: "set gateway.replicas: 2", Source: "values, PDB, Deployment"},
		{ID: "P7", Status: NA, Message: "no MCP Server", Source: "values, objects"},
	})
	want := "✗ P1  gateway.replicas is 1; production needs 2 or more [values, PDB, Deployment]\n      next: set gateway.replicas: 2\n" +
		"- P7  no MCP Server [values, objects]\n0 passed, 0 warnings, 1 failed, 1 n/a\n"
	if b.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}
}
