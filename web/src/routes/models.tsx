import { useModels } from "@/api";
import { Shell, Status } from "@/components/shell";

// The Model API spells providers as agentgateway does; people read the vendors' own names.
const providerName: Record<string, string> = {
  Anthropic: "Anthropic", Azure: "Azure OpenAI", Bedrock: "Amazon Bedrock", Gemini: "Google Gemini", OpenAI: "OpenAI", Openrouter: "OpenRouter", VertexAI: "Vertex AI",
};

export function ModelsPage() {
  const q = useModels();
  return (
    <Shell crumb="Govern / Models">
      <section className="p-5">
        <h1 className="text-xl font-semibold tracking-tight">Models</h1>
        <p className="mb-4 text-muted">The model catalog. A model is closed to everyone until an access rule names it.</p>
        {q.isError && <p className="text-bad">{q.error.message}</p>}
        <table className="w-full border-collapse">
          <thead>
            <tr className="border-b border-line text-left font-mono text-[10.5px] uppercase tracking-[.1em] text-muted">
              <th className="px-3 py-2.5 font-medium">Model</th><th className="px-3 py-2.5 font-medium">Name clients send</th>
              <th className="px-3 py-2.5 font-medium">Provider</th><th className="px-3 py-2.5 font-medium">Who can use it</th>
              <th className="px-3 py-2.5 font-medium">Status</th>
            </tr>
          </thead>
          <tbody>
            {q.data?.map((m) => (
              <tr key={m.name} className="border-b border-line-soft align-top">
                <td className="px-3 py-2.5"><span className="font-mono">{m.name}</span><span className="block text-[12px] text-muted">{m.displayName}</span></td>
                <td className="px-3 py-2.5 font-mono text-[12.5px]">{m.model}</td>
                <td className="px-3 py-2.5">{providerName[m.provider] ?? m.provider}</td>
                <td className="px-3 py-2.5">{m.allSignedIn ? "Everyone signed in" : m.allowedGroups.length ? m.allowedGroups.join(", ") : <span className="text-muted">Nobody yet</span>}</td>
                <td className="px-3 py-2.5"><Status ready={m.ready} /></td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>
    </Shell>
  );
}
