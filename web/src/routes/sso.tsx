import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { sendJSON, useConfig, useLoginOptions, useMe } from "@/api";
import { Button } from "@/components/ui/button";
import { Shell } from "@/components/shell";
import { cn } from "@/lib/utils";

type Check = { issuer: string; tokenEndpoint: string; groupsClaim: boolean; redirectURI: string };
type Result = { secretName: string; redirectURI: string; values: string };
const field = "w-full rounded-md border border-line bg-surface px-2.5 py-1.5 font-mono text-[13px] outline-none focus:border-accent";

// Connect SSO: the console checks the provider and keeps the client secret in a Kubernetes Secret;
// the connector itself is added to the Helm values, so Git or helm upgrade stays the one source.
export function SSOPage() {
  const me = useMe();
  const login = useLoginOptions();
  const config = useConfig();
  const [f, setF] = useState({ id: "", name: "", issuer: "", clientID: "", clientSecret: "", groups: true });
  const [copied, setCopied] = useState(false);
  const check = useMutation({ mutationFn: () => sendJSON("POST", "/api/sso/check", { issuer: f.issuer }) as Promise<Check> });
  const save = useMutation({
    mutationFn: () => sendJSON("POST", "/api/sso/connectors", f) as Promise<Result>,
    onSuccess: () => setF({ ...f, clientSecret: "" }), // do not keep the secret in the page
  });
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setF({ ...f, [k]: e.target.type === "checkbox" ? e.target.checked : e.target.value });
  const isAdmin = me.data?.role === "admin";
  const gitops = config.data?.mode === "gitops";

  return (
    <Shell crumb="Settings / Sign-in">
      <section className="max-w-3xl space-y-5 p-5">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">Sign-in</h1>
          <p className="text-muted">Connect your company identity provider (OpenID Connect) so people sign in with their own accounts and groups.</p>
        </div>
        <div>
          <h2 className="mb-1 font-mono text-[10.5px] uppercase tracking-[.12em] text-muted">Configured now</h2>
          <ul className="space-y-0.5">
            {login.data?.sso.map((c) => <li key={c.id}><span className="font-mono">{c.id}</span> · {c.name}</li>)}
            {login.data?.bootstrap && <li><span className="font-mono">local</span> · Bootstrap admin <span className="text-[12px] text-muted">(turn off once SSO works)</span></li>}
          </ul>
        </div>
        {!isAdmin && <p className="text-muted">Only admins can connect a provider.</p>}
        {isAdmin && !save.data && (
          <form className="space-y-3 rounded-md border border-line p-4" onSubmit={(e) => { e.preventDefault(); save.mutate(); }}>
            <h2 className="text-[15px] font-semibold">Connect a provider</h2>
            <label className="block"><span className="mb-1 block text-muted">Issuer URL</span>
              <div className="flex gap-2">
                <input id="sso-issuer" className={field} value={f.issuer} onChange={set("issuer")} placeholder="https://login.acme.com/realms/acme" required />
                <Button type="button" variant="outline" className="w-auto" disabled={!f.issuer || check.isPending} onClick={() => check.mutate()}>Check</Button>
              </div>
            </label>
            {check.isError && <p className="rounded-md border border-bad p-2 text-bad">{check.error.message}</p>}
            {check.data && (
              <div className="rounded-md border border-line bg-sunken p-3 text-[13px]" data-testid="sso-check">
                <p className="text-ok">The provider answers as <span className="font-mono">{check.data.issuer}</span>.</p>
                <p>Register this callback URL at the provider: <span className="font-mono">{check.data.redirectURI}</span></p>
                {!check.data.groupsClaim && <p className="text-muted">The provider does not list a groups claim in its discovery document. Many add groups through a claim mapper or an app setting (Keycloak, Entra ID); make sure it is set up, because roles and access rules use it.</p>}
              </div>
            )}
            <div className="grid grid-cols-2 gap-3">
              <label className="block"><span className="mb-1 block text-muted">ID</span>
                <input id="sso-id" className={field} value={f.id} onChange={set("id")} placeholder="okta" required /></label>
              <label className="block"><span className="mb-1 block text-muted">Button text</span>
                <input id="sso-name" className={cn(field, "font-sans")} value={f.name} onChange={set("name")} placeholder="Acme (Okta)" required /></label>
              <label className="block"><span className="mb-1 block text-muted">Client ID</span>
                <input id="sso-client-id" className={field} value={f.clientID} onChange={set("clientID")} required /></label>
              <label className="block"><span className="mb-1 block text-muted">Client secret</span>
                <input id="sso-client-secret" type="password" autoComplete="off" className={field} value={f.clientSecret} onChange={set("clientSecret")} required /></label>
            </div>
            <label className="flex items-center gap-2"><input id="sso-groups" type="checkbox" checked={f.groups} onChange={set("groups")} /> Read group membership (needed for roles and access rules)</label>
            {save.isError && <p className="rounded-md border border-bad p-2 text-bad">{save.error.message}</p>}
            <Button type="submit" className="w-auto" disabled={save.isPending}>Save secret and show values</Button>
            <p className="text-[12px] text-muted">The client secret goes straight into a Kubernetes Secret; it is not shown again or written to logs.</p>
          </form>
        )}
        {save.data && (
          <div className="space-y-3 rounded-md border border-line p-4" data-testid="sso-result">
            <h2 className="text-[15px] font-semibold">Saved the client secret in <span className="font-mono">{save.data.secretName}</span></h2>
            <p>Add these entries to <span className="font-mono">sso.connectors</span> and <span className="font-mono">dex.envVars</span> in your Ambit values file,
              then {gitops ? "commit the change to your configuration repository" : <>run <span className="font-mono">helm upgrade</span></>}. Dex restarts with the new sign-in option.</p>
            <pre className="overflow-x-auto rounded-md border border-line-soft bg-sunken p-3 font-mono text-[12.5px] leading-relaxed">{save.data.values}</pre>
            <div className="flex gap-2">
              <Button type="button" className="w-auto" onClick={() => navigator.clipboard.writeText(save.data!.values).then(() => setCopied(true))}>{copied ? "Copied" : "Copy values"}</Button>
              <Button type="button" variant="outline" className="w-auto" onClick={() => { save.reset(); check.reset(); setCopied(false); }}>Connect another</Button>
            </div>
            <p className="text-[12.5px] text-muted">Register <span className="font-mono">{save.data.redirectURI}</span> as the callback URL at the provider if you have not yet.</p>
          </div>
        )}
      </section>
    </Shell>
  );
}
