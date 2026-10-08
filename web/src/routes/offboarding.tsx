import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { Shell, Status } from "@/components/shell";

type GrantResult = { connection: string; revokedAtProvider: boolean; error?: string };
type OffboardingRecord = { email: string; at: string; trigger: string; grants: GrantResult[]; sessionsEnded: number; gatewayBlocked: boolean; completeSuccess: boolean };
type Offboarding = { scimURL: string; records: OffboardingRecord[] };

async function get<T>(url: string): Promise<T> {
  const r = await fetch(url, { credentials: "same-origin" });
  if (!r.ok) throw new Error((await r.json().catch(() => null))?.error ?? `${url} returned ${r.status}`);
  return r.json();
}

// Offboarding (journey A4): the identity provider deactivates someone over SCIM; Ambit revokes every
// grant they authorized at its provider, ends their sessions, blocks their tokens, and keeps this record.
export function OffboardingPage() {
  const q = useQuery({ queryKey: ["offboarding"], queryFn: () => get<Offboarding>("/api/offboarding") });
  const [token, setToken] = useState<string | null>(null);
  return (
    <Shell crumb="Settings / Offboarding">
      <section className="max-w-4xl space-y-5 p-5">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">Offboarding</h1>
          <p className="text-muted">When your identity provider deactivates someone, Ambit asks each app to revoke the grants they authorized, ends their console sessions, and blocks their gateway tokens at once. A revocation that an app does not confirm is listed below with its error.</p>
        </div>
        {q.isError && <p className="text-bad">{q.error.message}</p>}
        {q.data && (
          <div className="space-y-2 rounded-md border border-line p-4">
            <h2 className="font-semibold">Connect your identity provider (SCIM 2.0)</h2>
            <dl className="grid grid-cols-[120px_1fr] gap-x-3 gap-y-1.5">
              <dt className="text-muted">Base URL</dt><dd className="font-mono">{q.data.scimURL}</dd>
              <dt className="text-muted">Bearer token</dt>
              <dd>{token ? <span className="break-all font-mono">{token}</span>
                : <Button variant="outline" className="w-auto" onClick={() => get<{ token: string }>("/api/offboarding/token").then((t) => setToken(t.token))}>Show token</Button>}</dd>
            </dl>
            <p className="text-[12.5px] text-muted">In Okta or Entra ID, add a SCIM app with this URL and token, and turn on "Deactivate users".</p>
          </div>
        )}
        <div>
          <h2 className="mb-2 font-mono text-[10.5px] uppercase tracking-[.12em] text-muted">Records</h2>
          {q.data?.records.length === 0 && <p className="text-muted">Nobody has been offboarded yet.</p>}
          {q.data?.records.map((r) => (
            <div key={r.email + r.at} className="mb-3 rounded-md border border-line p-3" data-testid={`record-${r.email}`}>
              <p className="flex items-center justify-between"><span><b>{r.email}</b> · {new Date(r.at).toLocaleString()} · <span className="font-mono text-[12px]">{r.trigger}</span></span>
                <Status ready={r.completeSuccess ? "True" : "False"} text={r.completeSuccess ? "Complete" : "Needs attention"} /></p>
              <ul className="mt-1 text-[13px]">
                {r.grants.map((g) => (
                  <li key={g.connection}>{g.connection}: {g.revokedAtProvider ? "revoked at the provider" : g.error ? <span className="text-bad">{g.error}</span> : "forgotten (the provider has no revocation endpoint)"}</li>
                ))}
                {r.grants.length === 0 && <li className="text-muted">No app grants.</li>}
                <li>{r.sessionsEnded} console session(s) ended · gateway tokens {r.gatewayBlocked ? "blocked" : <span className="text-bad">not blocked</span>}</li>
              </ul>
            </div>
          ))}
        </div>
      </section>
    </Shell>
  );
}
