import { Navigate } from "@tanstack/react-router";
import { useMe } from "@/api";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Wordmark } from "@/components/wordmark";

export function LoginPage() {
  const me = useMe();
  if (me.data) return <Navigate to="/" />;
  return (
    <main className="grid min-h-screen place-items-center px-4">
      <Card className="w-full max-w-sm">
        <Wordmark />
        <h1 className="mt-6 text-lg font-semibold">Sign in</h1>
        <p className="mb-5 text-sm text-muted">Use your company account, or the bootstrap admin during first setup.</p>
        <Button variant="placeholder" disabled>
          Continue with company SSO — not connected yet
        </Button>
        <div className="my-4 flex items-center gap-3 font-mono text-[11px] uppercase tracking-widest text-muted before:h-px before:flex-1 before:bg-line after:h-px after:flex-1 after:bg-line">
          bootstrap admin
        </div>
        <Button asChild>
          <a href="/auth/login?connector=local">Sign in with bootstrap admin</a>
        </Button>
        <p className="mt-4 text-xs text-warn">
          The bootstrap admin is for first setup only. Turn it off after you connect SSO.
        </p>
      </Card>
    </main>
  );
}
