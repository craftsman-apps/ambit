import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { getJSON, isProposed, sendJSON, useConfig, type PullRequest } from "@/api";
import { Button } from "@/components/ui/button";
import { PullRequestNotice } from "@/components/pull-request";
import { providerName } from "@/lib/providers";

const input = "w-full rounded-md border border-line bg-surface px-2 py-1 font-mono text-small outline-none focus:border-accent";
const cloudOnly = (p: string) => p === "Bedrock" || p === "VertexAI";
const formats = ["Completions", "Messages", "Responses", "Embeddings"];
const nameRE = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/;
// The browser chooses the operation ID, so a retry of the same form repeats the same addition.
// getRandomValues, not randomUUID: randomUUID exists only in secure contexts, and the console may be served over http.
const newOperation = () => Array.from(crypto.getRandomValues(new Uint8Array(10)), (b) => b.toString(16).padStart(2, "0")).join("");

type Credentials = "entered" | "existing" | "cloud";
type Unfinished = { secret: string; model: string; operation: string; deleted?: boolean; pullRequest?: PullRequest & { closed: boolean; merged: boolean } };

function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return <div><label className="block"><span className="mb-1 block text-small text-muted">{label}</span>{children}</label>{hint && <p className="mt-1 text-small text-muted">{hint}</p>}</div>;
}

// The keys an admin types for a provider: an API key, AWS keys (Bedrock) or a Google key file (Vertex AI). They stay
// in this component's state until sent, and the caller clears them right after.
export type KeyValues = { apiKey: string; accessKey: string; secretKey: string; sessionToken: string; googleKeyFile: string; fileName: string };
export const emptyKeys: KeyValues = { apiKey: "", accessKey: "", secretKey: "", sessionToken: "", googleKeyFile: "", fileName: "" };

export function keyBody(provider: string, v: KeyValues): Record<string, unknown> {
  if (provider === "Bedrock") return { awsKeys: { accessKey: v.accessKey, secretKey: v.secretKey, ...(v.sessionToken && { sessionToken: v.sessionToken }) } };
  if (provider === "VertexAI") return { googleKeyFile: v.googleKeyFile };
  return { apiKey: v.apiKey };
}

export const enterLabel = (provider: string) =>
  provider === "Bedrock" ? "Enter the keys here" : provider === "VertexAI" ? "Upload the key file" : "Enter the key here";

export function KeyFields({ provider, v, set, fresh }: { provider: string; v: KeyValues; set: (v: KeyValues) => void; fresh?: boolean }) {
  const pre = fresh ? "New " : "";
  const secret = { className: input, type: "password", autoComplete: "off", spellCheck: false } as const;
  if (provider === "Bedrock") return (
    <div className="grid gap-3 sm:grid-cols-2">
      <Field label={`${pre}AWS access key ID`}><input {...secret} value={v.accessKey} onChange={(e) => set({ ...v, accessKey: e.target.value })} required /></Field>
      <Field label={`${pre}AWS secret access key`}><input {...secret} value={v.secretKey} onChange={(e) => set({ ...v, secretKey: e.target.value })} required /></Field>
      <Field label="AWS session token (optional)" hint="Only for temporary keys (access key ID ASIA...)."><input {...secret} value={v.sessionToken} onChange={(e) => set({ ...v, sessionToken: e.target.value })} /></Field>
    </div>
  );
  if (provider === "VertexAI") return (
    <Field label={`${pre}Google service-account key file`} hint={v.fileName ? `Selected: ${v.fileName}` : "A JSON file of type service_account, from Google Cloud IAM."}>
      <input className={input} type="file" accept=".json,application/json" required={!v.googleKeyFile}
        onChange={async (e) => { const f = e.target.files?.[0]; e.target.value = ""; if (f) set({ ...v, googleKeyFile: await f.text(), fileName: f.name }); }} />
    </Field>
  );
  return (
    <Field label={`${pre}API key`}>
      <input {...secret} value={v.apiKey} onChange={(e) => set({ ...v, apiKey: e.target.value })} required />
    </Field>
  );
}

// Add a model (admins). A typed API key is stored only in Secret ambit-llm-<name>; the server never returns it.
export function AddModel({ onDone }: { onDone: () => void }) {
  const qc = useQueryClient();
  const config = useConfig().data;
  const gitops = config?.mode === "gitops";
  const namespace = config?.namespace ?? "ambit-system";
  const [operation] = useState(newOperation);
  const [name, setName] = useState("");
  const [provider, setProvider] = useState("OpenAI");
  const [modelID, setModelID] = useState("");
  const [baseURL, setBaseURL] = useState("");
  const [fmts, setFmts] = useState<string[]>([]);
  const [azure, setAzure] = useState({ resourceName: "", resourceType: "OpenAI", apiVersion: "", projectName: "" });
  const [region, setRegion] = useState("");
  const [projectId, setProjectId] = useState("");
  const [choice, setChoice] = useState<Credentials>("entered");
  const [keys, setKeys] = useState<KeyValues>(emptyKeys);
  const [secretName, setSecretName] = useState("");
  const [pr, setPR] = useState<PullRequest | null>(null);
  const cloud = cloudOnly(provider);
  const credentials: Credentials = choice;
  const secrets = useQuery({ queryKey: ["model-secrets"], enabled: credentials === "existing", queryFn: () => getJSON<string[]>("/api/model-secrets") });
  const nameOK = nameRE.test(name);
  const save = useMutation({
    mutationFn: () => {
      const body: Record<string, unknown> = { operation, name, provider, model: modelID.trim(), credentials };
      if (provider === "OpenAI" && baseURL.trim()) { body.baseURL = baseURL.trim(); if (fmts.length) body.formats = fmts; }
      if (provider === "Azure") {
        body.azure = { resourceName: azure.resourceName, resourceType: azure.resourceType,
          ...(azure.apiVersion && { apiVersion: azure.apiVersion }), ...(azure.resourceType === "Foundry" && { projectName: azure.projectName }) };
      }
      if (provider === "Bedrock" && region) body.bedrock = { region };
      if (provider === "VertexAI") body.vertexai = { projectId, ...(region && { region }) };
      if (credentials === "entered") Object.assign(body, keyBody(provider, keys));
      if (credentials === "existing") body.secretName = secretName;
      return sendJSON("POST", "/api/models", body);
    },
    onSuccess: (res) => {
      setKeys(emptyKeys); // the key leaves the page as soon as it is sent
      qc.invalidateQueries({ queryKey: ["models"] }); qc.invalidateQueries({ queryKey: ["model-secrets"] }); qc.invalidateQueries({ queryKey: ["unfinished-models"] });
      if (isProposed(res)) setPR(res.pullRequest); else onDone();
    },
  });
  if (pr) return <PullRequestNotice pr={pr} repository={config?.repository} onDone={onDone} />;
  return (
    <form className="mb-4 max-w-2xl space-y-3 rounded-md border border-line p-3" aria-label="Add model"
      onSubmit={(e) => { e.preventDefault(); save.mutate(); }}>
      <div className="grid gap-3 sm:grid-cols-2">
        <Field label="Name" hint="Lowercase letters, digits and dashes.">
          <input className={input} value={name} onChange={(e) => setName(e.target.value)} required maxLength={63} autoComplete="off" aria-invalid={name !== "" && !nameOK} />
        </Field>
        <Field label="Provider">
          <select className={input} value={provider} onChange={(e) => setProvider(e.target.value)}>
            {Object.entries(providerName).map(([id, label]) => <option key={id} value={id}>{label}</option>)}
          </select>
        </Field>
        <Field label="Model ID" hint="The name clients send in their requests.">
          <input className={input} value={modelID} onChange={(e) => setModelID(e.target.value)} required autoComplete="off" />
        </Field>
        {provider === "OpenAI" && (
          <Field label="Base URL" hint="Only for a self-hosted, OpenAI-compatible server, with its API path, like http://vllm.models.svc:8000/v1.">
            <input className={input} value={baseURL} onChange={(e) => setBaseURL(e.target.value)} placeholder="optional" autoComplete="off" />
          </Field>
        )}
      </div>
      {provider === "OpenAI" && baseURL.trim() !== "" && (
        <fieldset className="text-small"><legend className="mb-1 text-muted">Formats the server implements (optional)</legend>
          <div className="flex flex-wrap gap-x-4 gap-y-1">
            {formats.map((f) => (
              <label key={f} className="flex items-center gap-2">
                <input type="checkbox" checked={fmts.includes(f)} onChange={(e) => setFmts(e.target.checked ? [...fmts, f] : fmts.filter((x) => x !== f))} /> {f}
              </label>
            ))}
          </div>
        </fieldset>
      )}
      {provider === "Azure" && (
        <div className="grid gap-3 sm:grid-cols-2">
          <Field label="Azure resource name"><input className={input} value={azure.resourceName} onChange={(e) => setAzure({ ...azure, resourceName: e.target.value })} required /></Field>
          <Field label="Azure resource type">
            <select className={input} value={azure.resourceType} onChange={(e) => setAzure({ ...azure, resourceType: e.target.value })}>
              <option value="OpenAI">OpenAI</option><option value="Foundry">Foundry</option>
            </select>
          </Field>
          <Field label="API version (optional)"><input className={input} value={azure.apiVersion} onChange={(e) => setAzure({ ...azure, apiVersion: e.target.value })} placeholder="v1" /></Field>
          {azure.resourceType === "Foundry" && <Field label="Foundry project name"><input className={input} value={azure.projectName} onChange={(e) => setAzure({ ...azure, projectName: e.target.value })} required /></Field>}
        </div>
      )}
      {provider === "Bedrock" && <Field label="AWS region (optional)" hint="Default us-east-1."><input className={input} value={region} onChange={(e) => setRegion(e.target.value)} placeholder="eu-west-1" /></Field>}
      {provider === "VertexAI" && (
        <div className="grid gap-3 sm:grid-cols-2">
          <Field label="Google Cloud project ID"><input className={input} value={projectId} onChange={(e) => setProjectId(e.target.value)} required /></Field>
          <Field label="Region (optional)" hint="Default global."><input className={input} value={region} onChange={(e) => setRegion(e.target.value)} placeholder="europe-west4" /></Field>
        </div>
      )}

      <fieldset className="space-y-2">
        <legend className="mb-1 font-medium">How the provider key is given</legend>
        <label className="flex items-center gap-2"><input type="radio" name="credentials" checked={credentials === "entered"} onChange={() => setChoice("entered")} /> {enterLabel(provider)}</label>
        {credentials === "entered" && (
          <div className="ml-6 space-y-2">
            <KeyFields provider={provider} v={keys} set={setKeys} />
            <div className="space-y-1 rounded-md border border-line-soft bg-bg p-2 text-small text-muted" data-testid="key-caveat">
              <p>The key is stored in the Kubernetes Secret <span className="font-mono">ambit-llm-{name || "<name>"}</span> in the namespace <span className="font-mono">{namespace}</span>. Anyone who can read Secrets in this namespace can read the key. The console never shows the key again. For production, create the Secret with your secret manager (for example External Secrets) and choose <strong>Use an existing Secret</strong>.</p>
              {gitops && <p>The key is not put in Git. The console writes the Secret to the cluster now; the pull request adds only the Model. If you rebuild the cluster from Git, create the Secret again.</p>}
            </div>
          </div>
        )}
        <label className="flex items-center gap-2"><input type="radio" name="credentials" checked={credentials === "existing"} onChange={() => setChoice("existing")} /> Use an existing Secret</label>
        {credentials === "existing" && (
          <div className="ml-6 space-y-2">
            <Field label="Secret">
              <select className={input} value={secretName} onChange={(e) => setSecretName(e.target.value)} required>
                <option value="">Choose a Secret</option>
                {secrets.data?.map((n) => <option key={n} value={n}>{n}</option>)}
              </select>
            </Field>
            {secrets.isError && <p className="text-small text-bad">{secrets.error.message}</p>}
            {secrets.data?.length === 0 && <p className="text-small text-muted">No Secret named <span className="font-mono">ambit-llm-*</span> yet.</p>}
            <p className="text-small text-muted">Create one with: <code className="font-mono">kubectl -n {namespace} create secret generic ambit-llm-{"<name>"} --from-literal=Authorization=&lt;api key&gt;</code>{cloud && " (Bedrock keys: accessKey and secretKey; Vertex AI: credentials.json — see the provider docs)"}</p>
          </div>
        )}
        {cloud && (
          <label className="flex items-center gap-2"><input type="radio" name="credentials" checked={credentials === "cloud"} onChange={() => setChoice("cloud")} /> Use the gateway's cloud identity</label>
        )}
      </fieldset>

      <div className="flex gap-2">
        <Button type="submit" className="w-auto" disabled={save.isPending || !nameOK}>Create model</Button>
        <Button type="button" variant="outline" className="w-auto" onClick={onDone}>Cancel</Button>
      </div>
      {save.isError && <p className="text-small text-bad" role="alert">{save.error.message}</p>}
    </form>
  );
}

// Keys the console stored for a model that was never added (the pull request was closed, or a step failed), or whose
// model was deleted while the key stayed (GitOps mode, or a failed removal).
export function UnfinishedModels() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["unfinished-models"], queryFn: () => getJSON<Unfinished[]>("/api/models/unfinished") });
  const remove = useMutation({
    mutationFn: (secret: string) => sendJSON("DELETE", `/api/models/unfinished/${encodeURIComponent(secret)}`),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["unfinished-models"] }); qc.invalidateQueries({ queryKey: ["model-secrets"] }); },
  });
  if (!q.data?.length) return null;
  return (
    <div className="mb-4 max-w-2xl rounded-md border border-line p-3" data-testid="unfinished-models">
      <p className="font-medium">Keys without a model</p>
      <p className="mb-2 text-small text-muted">The console stored these keys, but their model was not added or was deleted.</p>
      <ul className="space-y-1">
        {q.data.map((u) => (
          <li key={u.secret} className="flex flex-wrap items-center gap-2 text-small">
            <span className="font-mono">{u.model}</span>{u.deleted && <span className="text-muted">(deleted)</span>}<span className="text-muted">key in <span className="font-mono">{u.secret}</span></span>
            <Button variant="outline" className="w-auto" disabled={remove.isPending} onClick={() => remove.mutate(u.secret)}>Remove key</Button>
          </li>
        ))}
      </ul>
      {remove.isError && <p className="text-small text-bad" role="alert">{remove.error.message}</p>}
    </div>
  );
}
