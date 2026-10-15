// Audit-only mode, as the console shows it. Pure functions, so they run without a browser.

export type Outcome = "refused" | "would-refuse" | "allow" | "other-model" | "mark-missing" | "n/a";

// Whole days from an RFC 3339 time to now (UTC); never negative.
export function daysSince(iso: string, now: Date): number {
  return Math.max(0, Math.floor((now.getTime() - new Date(iso).getTime()) / 864e5));
}
export const dayCount = (n: number) => n === 0 ? "today" : n === 1 ? "1 day" : `${n} days`;

const utc = (iso: string) => new Date(iso).toISOString().replace("T", " ").slice(0, 16);
export { utc as utcMinute };

export function modeBanner(since: string | undefined, now: Date): string {
  const when = since ? ` since ${since.slice(0, 10)} (${dayCount(daysSince(since, now))})` : "";
  return `Audit-only mode${when}.`;
}

export type Pending = { mode: string; rulesVersion: string; step: string; error?: string; objects?: string[] };
// The banner for a change that agentgateway has not applied: a mode switch, or a rules change in the same mode.
export function pendingBanner(p: Pending, published: string): string {
  const what = p.mode !== published ? `Mode switch to ${p.mode}` : `Rules change ${p.rulesVersion}`;
  return `${what} is not complete: ${p.error || `waiting for agentgateway (${p.step})`}`;
}

// The console page of the objects agentgateway rejected, when there is one: only a Model has a page (the list).
export function pendingPage(p: Pending): { to: string; label: string } | undefined {
  return p.objects?.some((o) => o.startsWith("Model/")) ? { to: "/models", label: "Open the Models page" } : undefined;
}

// The labels of the Outcome column for an audit-mode record; undefined: the page's existing outcome applies.
export function outcomeLabel(outcome: Outcome | undefined, status: string): { text: string; tone: "bad" | "warn"; note?: string } | undefined {
  switch (outcome) {
    case "refused": return { text: `Refused (${status})`, tone: "bad" };
    case "would-refuse": return { text: `Would be refused (${status})`, tone: "warn" };
    case "other-model": return { text: "Would use another model (unless a later route matched)", tone: "warn" };
    case "mark-missing": return { text: "Mark missing", tone: "warn", note: "The rule expression failed, or the audit fields were not active yet." };
    default: return undefined;
  }
}

export type WouldRefuse = {
  from: string; to: string; rules: "all" | "current"; rulesVersion?: string;
  requests: number; people: number; markMissing: number; refused: number; newest: string | null;
  groups: { group: string; target: string; outcome: string; requests: number; people: number }[]; limited: boolean;
};

// "No record in the last hour" while audit-only is on (records reach the store within about a minute).
export const isStale = (newest: string | null, now: Date) => !newest || now.getTime() - new Date(newest).getTime() > 36e5;

// §5.3: what the summary says about switching to enforcing, for the current rules only. The rules must have
// been in force 7 days for "nothing seen" to mean anything.
export function switchCheck(s: WouldRefuse, rulesSince: string | undefined, now: Date): string | undefined {
  if (s.rules !== "current" || !rulesSince) return undefined;
  const age = daysSince(rulesSince, now);
  if (age < 7) return `The current rules have been in force for ${age === 0 ? "less than 1 day" : dayCount(age)}. Wait until they have been in force for 7 days before relying on this count.`;
  if (s.requests === 0 && s.markMissing === 0) return `Switching to enforcing now would refuse nothing seen since ${rulesSince.slice(0, 16).replace("T", " ")} UTC.`;
  return undefined;
}

// What the pages say about a model or a tool that no access rule names: closed in enforcing mode, let through (and
// marked) in audit-only mode.
export function closedWording(audit: boolean) {
  if (audit) return {
    rules: "Who may use which models and MCP tools. Audit-only mode: anything no rule names is let through and marked as would be refused.",
    noRules: "No access rules yet. In audit-only mode every model is let through and marked as would be refused.",
    models: "The model catalog. In audit-only mode a model that no access rule names is let through and marked as would be refused. Prices estimate cost and drive dollar budgets.",
    servers: "Servers run in their own pods, or elsewhere and are reached by URL; either way, only through the gateway. In audit-only mode their tools are let through, and a tool that no access rule names is marked as would be refused.",
    noServerRule: "None: would be refused",
    count: (n: number) => `${n} named by no rule (let through)`,
  };
  return {
    rules: "Who may use which models and MCP tools. Anything no rule names is closed to everyone.",
    noRules: "No access rules yet, so no model can be used.",
    models: "The model catalog. A model is closed to everyone until an access rule names it. Prices estimate cost and drive dollar budgets.",
    servers: "Servers run in their own pods, or elsewhere and are reached by URL; either way, only through the gateway. Their tools are closed to everyone until an access rule names them.",
    noServerRule: "None: closed to everyone",
    count: (n: number) => `${n} closed to everyone`,
  };
}
