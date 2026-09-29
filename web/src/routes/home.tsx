import { Navigate } from "@tanstack/react-router";
import { useMe } from "@/api";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Wordmark } from "@/components/wordmark";

export function HomePage() {
  const me = useMe();
  if (me.isPending) return null;
  if (me.isError) return <p className="p-8 text-sm">Could not load your session: {me.error.message}</p>;
  if (!me.data) return <Navigate to="/login" />;
  const u = me.data;
  return (
    <div className="min-h-screen">
      <header className="flex items-center justify-between border-b border-line bg-surface px-6 py-3">
        <Wordmark />
        <div className="flex items-center gap-4 text-sm">
          <span className="text-muted">{u.email}</span>
          <span className="rounded border border-line px-2 py-0.5 font-mono text-xs">{u.role}</span>
          <form method="post" action="/auth/logout">
            <Button variant="outline" type="submit" className="w-auto">Sign out</Button>
          </form>
        </div>
      </header>
      <main className="mx-auto max-w-3xl space-y-4 px-6 py-10">
        <h1 className="text-xl font-semibold" data-testid="signed-in-as">
          Signed in as {u.bootstrap ? "admin (bootstrap)" : u.name || u.email}
        </h1>
        {u.bootstrap && (
          <Card className="border-warn bg-warn-soft text-sm">
            You are using the bootstrap admin. Connect your company identity provider, then turn this account off.
          </Card>
        )}
      </main>
    </div>
  );
}
