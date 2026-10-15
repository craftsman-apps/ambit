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

export type Pending = { mode: string; rulesVersion: string; step: string; error?: string };
// The banner for a change that agentgateway has not applied: a mode switch, or a rules change in the same mode.
export function pendingBanner(p: Pending, published: string): string {
  const what = p.mode !== published ? `Mode switch to ${p.mode}` : `Rules change ${p.rulesVersion}`;
  return `${what} is not complete: ${p.error || `waiting for agentgateway (${p.step})`}`;
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
