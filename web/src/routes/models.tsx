import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { getJSON, isProposed, sendJSON, useConfig, useMe, useModels, type ModelPrice, type ModelView, type PriceInfo, type PullRequest } from "@/api";
import { Button } from "@/components/ui/button";
import { PullRequestNotice } from "@/components/pull-request";
import { Shell, Status } from "@/components/shell";
import { closedWording } from "@/lib/audit-mode";

// The Model API spells providers as agentgateway does; people read the vendors' own names.
const providerName: Record<string, string> = {
  Anthropic: "Anthropic", Azure: "Azure OpenAI", Baseten: "Baseten", Bedrock: "Amazon Bedrock", Cerebras: "Cerebras",
  Cohere: "Cohere", Deepinfra: "DeepInfra", Deepseek: "DeepSeek", Fireworks: "Fireworks AI", Gemini: "Google Gemini",
  Groq: "Groq", Huggingface: "Hugging Face", Meta: "Meta Llama API", Mistral: "Mistral AI", OpenAI: "OpenAI",
  Openrouter: "OpenRouter", TogetherAI: "Together AI", VertexAI: "Google Vertex AI", XAI: "xAI",
};
const field = "w-28 rounded-md border border-line bg-surface px-2 py-1 font-mono text-small outline-none focus:border-accent";
const priceText = (p: ModelPrice) => `$${p.inputPerMillionTokens} / $${p.outputPerMillionTokens} per million`;
// The controller appends "refused for groups …: no price" to the Ready message when a dollar budget applies.
const refusedText = (message: string) => message.match(/refused for groups [^;]*/)?.[0];

// Edit a model's price (admins): type it, use LiteLLM's suggestion, or remove it. Saving changes the Model
// in Direct mode, or opens a pull request in GitOps mode.
function PriceEditor({ m, onDone }: { m: ModelView; onDone: () => void }) {
  const qc = useQueryClient();
  const repository = useConfig().data?.repository;
  const info = useQuery({ queryKey: ["price", m.name], queryFn: () => getJSON<PriceInfo>(`/api/models/${encodeURIComponent(m.name)}/price`) });
  const [input, setInput] = useState(m.price?.inputPerMillionTokens ?? "");
  const [output, setOutput] = useState(m.price?.outputPerMillionTokens ?? "");
  const [pr, setPR] = useState<PullRequest | null>(null);
  const save = useMutation({
    mutationFn: (body: object) => sendJSON("PUT", `/api/models/${encodeURIComponent(m.name)}/price`, body),
    onSuccess: (res) => { qc.invalidateQueries({ queryKey: ["models"] }); if (isProposed(res)) setPR(res.pullRequest); else onDone(); },
  });
  if (pr) return <PullRequestNotice pr={pr} repository={repository} onDone={onDone} />;
  const s = info.data?.suggestion;
  return (
    <form className="mt-2 space-y-2 rounded-md border border-line p-2" aria-label={`Price of ${m.name}`}
      onSubmit={(e) => { e.preventDefault(); save.mutate({ inputPerMillionTokens: input.trim(), outputPerMillionTokens: output.trim() }); }}>
      <div className="flex flex-wrap items-end gap-2">
        <label className="block"><span className="mb-1 block text-small text-muted">Input $ per million</span>
          <input id={`price-in-${m.name}`} inputMode="decimal" className={field} value={input} onChange={(e) => setInput(e.target.value)} required /></label>
        <label className="block"><span className="mb-1 block text-small text-muted">Output $ per million</span>
          <input id={`price-out-${m.name}`} inputMode="decimal" className={field} value={output} onChange={(e) => setOutput(e.target.value)} required /></label>
        <Button type="submit" className="w-auto" disabled={save.isPending}>Save price</Button>
        {m.price && <Button type="button" variant="outline" className="w-auto" disabled={save.isPending} onClick={() => save.mutate({ remove: true })}>Remove price</Button>}
        <Button type="button" variant="outline" className="w-auto" onClick={onDone}>Cancel</Button>
      </div>
      {s && (
        <p className="flex flex-wrap items-center gap-2 text-small text-muted">
          <span>Suggested: {priceText(s)} (LiteLLM list of {s.date}, {s.source})</span>
          <Button type="button" variant="outline" className="w-auto" disabled={save.isPending}
            onClick={() => save.mutate({ inputPerMillionTokens: s.inputPerMillionTokens, outputPerMillionTokens: s.outputPerMillionTokens })}>Use this price</Button>
        </p>
      )}
      {info.data && !s && <p className="text-small text-muted">No suggestion for this model.</p>}
      {info.data?.listError && <p className="text-small text-warn">The price mirror could not be read ({info.data.listError}); suggestions come from the list built into this release.</p>}
      {save.isError && <p className="text-small text-bad" role="alert">{save.error.message}</p>}
    </form>
  );
}

export function ModelsPage() {
  const q = useModels();
  const admin = useMe().data?.role === "admin";
  const audit = useConfig().data?.accessMode === "audit";
  const [editing, setEditing] = useState<string | null>(null);
  return (
    <Shell crumb="Govern / Models">
      <section className="p-5">
        <h1 className="text-h1 font-semibold tracking-tight">Models</h1>
        <p className="mb-4 text-muted">{closedWording(audit).models}</p>
        {q.isError && <p className="text-bad">{q.error.message}</p>}
        <table className="w-full border-collapse">
          <thead>
            <tr className="border-b border-line text-left font-mono text-label uppercase tracking-[.1em] text-muted">
              <th className="px-3 py-2.5 font-medium">Model</th><th className="px-3 py-2.5 font-medium">Name clients send</th>
              <th className="px-3 py-2.5 font-medium">Provider</th><th className="px-3 py-2.5 font-medium">Who can use it</th>
              <th className="px-3 py-2.5 font-medium">Price</th><th className="px-3 py-2.5 font-medium">Status</th>
            </tr>
          </thead>
          <tbody>
            {q.data?.map((m) => (
              <tr key={m.name} className="border-b border-line-soft align-top" data-testid={`model-${m.name}`}>
                <td className="px-3 py-2.5"><span className="font-mono">{m.name}</span><span className="block text-small text-muted">{m.displayName}</span></td>
                <td className="px-3 py-2.5 font-mono text-small">{m.model}</td>
                <td className="px-3 py-2.5">{providerName[m.provider] ?? m.provider}</td>
                <td className="px-3 py-2.5">{m.allSignedIn ? "Everyone signed in" : m.allowedGroups.length || m.allowedUsers.length ? <>
                  {m.allowedGroups.join(", ")}
                  {m.allowedUsers.length > 0 && <span className="block font-mono text-small">{m.allowedUsers.join(", ")}</span>}</> : <span className="text-muted">Nobody yet</span>}</td>
                <td className="px-3 py-2.5">
                  {m.price ? <span className="font-mono text-small">{priceText(m.price)}</span> : <span className="text-muted">No price</span>}
                  {refusedText(m.message) && <span className="block text-small text-warn">{refusedText(m.message)}</span>}
                  {admin && editing !== m.name && <Button variant="outline" className="mt-1 block w-auto" onClick={() => setEditing(m.name)}>Edit price</Button>}
                  {editing === m.name && <PriceEditor m={m} onDone={() => setEditing(null)} />}
                </td>
                <td className="px-3 py-2.5"><Status ready={m.ready} /></td>
              </tr>
            ))}
          </tbody>
        </table>
        <p className="mt-3 text-small text-muted">Price suggestions come from LiteLLM's model price list (<a className="underline" href="/api/prices/licence">LiteLLM licence</a>).</p>
      </section>
    </Shell>
  );
}
