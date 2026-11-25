// Agents (docs/agents.md, experimental in 0.6.x). Pure helpers, so they run without a browser.

export type AgentMode = "person" | "itself";
export type AgentState = "pending" | "active" | "stopping" | "stopped" | "deleting";
export type AgentView = {
  name: string; displayName: string; owner: string; mode: AgentMode; group?: string; identity?: string;
  platform: string; scope: string[]; connectionConsent: string[]; state: AgentState; rev: number;
  created: string; createdBy: string; manageable: boolean;
};
export type AgentSettings = { enabled: boolean; maxTotal: number; maxPerPerson: number; keyMaxDays: number };

// The platform values the design lists. Any other lower-case word is kept and shown as given.
export const knownPlatforms: [string, string][] = [
  ["chatgpt", "ChatGPT"], ["claude-ai", "Claude.ai"], ["gemini-enterprise", "Gemini Enterprise"], ["copilot-studio", "Copilot Studio"],
  ["grok", "Grok Bot"], ["agentcore", "AWS AgentCore"], ["cloudflare", "Cloudflare Agents"], ["orca", "Orca"], ["other", "Other"],
];
export const PLATFORM_RE = /^[a-z0-9][a-z0-9._-]{0,39}$/;
export const NAME_RE = /^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$/;
export const platformText = (p: string) => knownPlatforms.find(([v]) => v === p)?.[1] ?? p;

export const stateText: Record<AgentState, string> = {
  pending: "Being set up", active: "Active", stopping: "Stopping", stopped: "Stopped", deleting: "Being deleted",
};

export const modeText = (m: AgentMode) => (m === "itself" ? "Itself (service account)" : "Person (acts as its owner)");

// "none", "2 servers, 1 tool". An empty scope lets the agent use no tool.
export function scopeSummary(scope: string[]): string {
  const servers = scope.filter((e) => !e.includes("/")).length;
  const tools = scope.length - servers;
  const part = (n: number, w: string) => `${n} ${w}${n === 1 ? "" : "s"}`;
  if (scope.length === 0) return "none";
  return [servers && part(servers, "server"), tools && part(tools, "tool")].filter(Boolean).join(", ");
}

// A new scope entry list from checkbox state: a whole-server entry replaces that server's tool entries.
export function toggleScope(scope: string[], entry: string): string[] {
  const server = entry.split("/")[0];
  if (scope.includes(entry)) return scope.filter((e) => e !== entry);
  const whole = !entry.includes("/");
  return [...scope.filter((e) => !whole || e.split("/")[0] !== server), entry];
}

// One CIDR per line or separated by commas or spaces; the console checks the ranges.
export const parseRanges = (text: string): string[] => text.split(/[\s,]+/).filter(Boolean);

// The newest use of any key of each agent (ISO times sort as text). A key list has no agent without a key.
export function lastUseByAgent(keys: { agent?: string; lastUsed: string | null }[]): Map<string, string> {
  const m = new Map<string, string>();
  for (const k of keys) if (k.agent && k.lastUsed && (m.get(k.agent) ?? "") < k.lastUsed) m.set(k.agent, k.lastUsed);
  return m;
}

// Tool calls of one agent from Audit: "server · tool" with a count, most used first.
export function toolsUsed(calls: { method: string; server: string; tool: string }[]): [string, number][] {
  const n = new Map<string, number>();
  for (const c of calls) if (c.method === "tools/call" && c.tool) { const k = c.server ? `${c.server} · ${c.tool}` : c.tool; n.set(k, (n.get(k) ?? 0) + 1); }
  return [...n.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
}

// The header the platform sends. Both forms are accepted by the gateway; the key is shown once.
export const headerLines = (key: string) => [`Authorization: Bearer ${key}`, `x-api-key: ${key}`];
