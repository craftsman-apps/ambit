import { Navigate } from "@tanstack/react-router";
import { useLoginOptions, useMe } from "@/api";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Wordmark } from "@/components/wordmark";

export function LoginPage() {
  const me = useMe();
  const opts = useLoginOptions();
  if (me.data) return <Navigate to="/" />;
  const sso = opts.data?.sso ?? [];
  return (
    <main className="grid min-h-screen place-items-center px-4">
      <Card className="w-full max-w-sm">
        <Wordmark />
        <h1 className="mt-6 text-lg font-semibold">Sign in</h1>
        <p className="mb-5 text-sm text-muted">
          {opts.data?.bootstrap ? "Use your company account, or the bootstrap admin during first setup." : "Use your company account."}
        </p>
        {opts.isError && <p className="mb-4 text-sm text-warn">Could not load sign-in options. Reload the page.</p>}
        <div className="space-y-2">
          {sso.map((c) => (
            <Button key={c.id} asChild variant={opts.data?.bootstrap ? "outline" : "primary"}>
              <a href={`/auth/login?connector=${encodeURIComponent(c.id)}`}>Continue with {c.name}</a>
            </Button>
          ))}
          {opts.data && sso.length === 0 && (
            <Button variant="placeholder" disabled>
              Company SSO is not connected yet
            </Button>
          )}
        </div>
        {opts.data?.bootstrap && (
          <>
            <div className="my-4 flex items-center gap-3 font-mono text-[11px] uppercase tracking-widest text-muted before:h-px before:flex-1 before:bg-line after:h-px after:flex-1 after:bg-line">
              bootstrap admin
            </div>
            <Button asChild variant={sso.length ? "outline" : "primary"}>
              <a href="/auth/login?connector=local">Sign in with bootstrap admin</a>
            </Button>
            <p className="mt-4 text-xs text-warn">
              The bootstrap admin is for first setup only. Turn it off after you connect SSO.
            </p>
          </>
        )}
      </Card>
    </main>
  );
}
