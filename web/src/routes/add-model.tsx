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
const newOperation = () => crypto.randomUUID().replaceAll("-", "").slice(0, 20);

type Credentials = "entered" | "existing" | "cloud";
type Unfinished = { secret: string; model: string; operation: string; pullRequest?: PullRequest & { closed: boolean; merged: boolean } };

function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return <div><label className="block"><span className="mb-1 block text-small text-muted">{label}</span>{children}</label>{hint && <p className="mt-1 text-small text-muted">{hint}</p>}</div>;
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
  const [apiKey, setApiKey] = useState("");
  const [secretName, setSecretName] = useState("");
  const [pr, setPR] = useState<PullRequest | null>(null);
  const cloud = cloudOnly(provider);
  const credentials: Credentials = cloud && choice === "entered" ? "existing" : choice;
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
      if (credentials === "entered") body.apiKey = apiKey;
      if (credentials === "existing") body.secretName = secretName;
      return sendJSON("POST", "/api/models", body);
    },
    onSuccess: (res) => {
      setApiKey(""); // the key leaves the page as soon as it is sent
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
        {!cloud && (
          <label className="flex items-center gap-2"><input type="radio" name="credentials" checked={credentials === "entered"} onChange={() => setChoice("entered")} /> Enter the key here</label>
        )}
        {credentials === "entered" && (
          <div className="ml-6 space-y-2">
            <Field label="API key">
              <input className={input} type="password" autoComplete="off" spellCheck={false} value={apiKey} onChange={(e) => setApiKey(e.target.value)} required />
            </Field>
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

// Keys the console stored for a model that was never added (the pull request was closed, or a step failed).
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
      <p className="font-medium">Unfinished additions</p>
      <p className="mb-2 text-small text-muted">The console stored these keys, but the model was not added.</p>
      <ul className="space-y-1">
        {q.data.map((u) => (
          <li key={u.secret} className="flex flex-wrap items-center gap-2 text-small">
            <span className="font-mono">{u.model}</span><span className="text-muted">key in <span className="font-mono">{u.secret}</span></span>
            <Button variant="outline" className="w-auto" disabled={remove.isPending} onClick={() => remove.mutate(u.secret)}>Remove key</Button>
          </li>
        ))}
      </ul>
      {remove.isError && <p className="text-small text-bad" role="alert">{remove.error.message}</p>}
    </div>
  );
}
