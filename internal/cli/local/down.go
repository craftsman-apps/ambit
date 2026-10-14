package local

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/craftsman-apps/ambit/internal/cli/kube"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// Down runs `ambit local down` (spec 4.2): it deletes the kind cluster only with both ownership proofs.
func Down(ctx context.Context, p *out.Printer, e *Env, name string, in io.Reader, interactive, yes bool) error {
	if _, err := e.ReadyDocker(); err != nil {
		return err
	}
	own, err := e.Owner(ctx, name)
	if err != nil {
		return err
	}
	switch own.Verdict {
	case Absent:
		if err := e.Store.Remove(name); err != nil {
			return err
		}
		p.Line("%s no kind cluster named %q; nothing to delete", p.Mark("ok"), name)
		return nil
	case NotOwned:
		return own.Refusal(name)
	}
	if err := kube.Confirm(p, in, interactive, yes, []string{
		fmt.Sprintf("kind cluster: %s (kubectl context %s)", name, KubeContext(name)),
	}, fmt.Sprintf("Delete the kind cluster %q and everything in it?", name)); err != nil {
		return err
	}
	start := time.Now()
	if err := e.Kind.Delete(name, e.Kubeconfig); err != nil {
		p.Step(false, "Deleted kind cluster "+name, time.Since(start))
		return err
	}
	if err := e.Store.Remove(name); err != nil {
		return err
	}
	p.Step(true, "Deleted kind cluster "+name, time.Since(start))
	return nil
}

// Password prints the bootstrap admin password of the local cluster (spec 4.2).
func Password(ctx context.Context, p *out.Printer, e *Env, name string) error {
	if _, err := e.ReadyDocker(); err != nil {
		return err
	}
	own, err := e.Owner(ctx, name)
	if err != nil {
		return err
	}
	switch own.Verdict {
	case Absent:
		return &out.Error{What: fmt.Sprintf("no kind cluster named %q", name), Why: "there is nothing to read", Next: "ambit local up" + nameFlag(name)}
	case NotOwned:
		return own.Refusal(name)
	}
	ops, err := e.OpenOps(e.Kubeconfig, KubeContext(name))
	if err != nil {
		return err
	}
	pw, err := ops.AdminPassword(ctx)
	if err != nil {
		return &out.Error{What: "the bootstrap admin password cannot be read", Why: err.Error(),
			Next: "the Secret ambit-initial-admin exists only while bootstrapAdmin.enabled is true"}
	}
	p.Line("%s", pw) // the one command whose purpose is to print it; it is not registered with the redactor
	return nil
}
