import { useQuery } from "@tanstack/react-query";

export type Me = {
  email: string;
  name: string;
  role: "admin" | "approver" | "viewer";
  groups?: string[];
  bootstrap: boolean;
};

export type LoginOptions = { bootstrap: boolean; namespace: string; sso: { id: string; name: string }[] };

// null means "not signed in"; any other failure is an error.
export function useMe() {
  return useQuery({
    queryKey: ["me"],
    retry: false,
    queryFn: async (): Promise<Me | null> => {
      const r = await fetch("/api/me", { credentials: "same-origin" });
      if (r.status === 401) return null;
      if (!r.ok) throw new Error(`/api/me returned ${r.status}`);
      return r.json();
    },
  });
}

export function useLoginOptions() {
  return useQuery({
    queryKey: ["login-options"],
    queryFn: async (): Promise<LoginOptions> => {
      const r = await fetch("/api/login-options");
      if (!r.ok) throw new Error(`/api/login-options returned ${r.status}`);
      return r.json();
    },
  });
}

export type ModelPrice = { inputPerMillionTokens: string; outputPerMillionTokens: string };
export type ModelView = {
  name: string; displayName: string; model: string; provider: string;
  allowedGroups: string[]; allSignedIn: boolean; ready: string; message: string;
  price: ModelPrice | null;
};
// A model's price and the suggestion from LiteLLM's list (admins only).
export type PriceInfo = {
  price: ModelPrice | null;
  suggestion: (ModelPrice & { key: string; source: string; commit: string; date: string }) | null;
  listError: string; licenceURL: string;
};
// One condition on one argument of a tool call (requirement 8); ambit-guard enforces it.
export type ArgumentRule = { name: string; oneOf?: string[]; notContains?: string[]; max?: string };
export type ServerAccess = { name: string; tools?: string[]; arguments?: ArgumentRule[] };
export type RuleView = {
  name: string; groups: string[]; allSignedIn: boolean; models: string[]; servers: ServerAccess[];
  ready: string; reason: string; message: string; gitManaged: boolean; createdBy: string;
};
export type RuleDetail = RuleView & {
  effective: { model: string; modelName: string; cel: string }[];
  effectiveServers: { server: string; cel: string }[];
};
export type RuleInput = { name?: string; groups: string[]; allSignedIn: boolean; models: string[]; servers: ServerAccess[] };

export type Tool = { name: string; description?: string; readOnlyHint?: boolean; destructiveHint?: boolean };
export type ServerView = {
  name: string; displayName: string; image: string; url: string; connection: string; transport: string; ready: string; message: string;
  tools: Tool[]; reader?: string; toolsReadAt?: string; toolsRead?: string; toolsReadMessage?: string; requestedBy: string; approvedBy: string; gitManaged: boolean; rules: string[]; changedTools: string[];
};
export type RequestView = {
  name: string; serverName: string; displayName: string; image: string; url: string; connection: string; transport: string; reason: string;
  requestedBy: string; requestedAt: string; decision: "" | "Approved" | "Denied"; decidedBy: string; decidedAt: string | null; comment: string;
  pullRequest: string;
  // GitOps: set while an approval waits for its pull request; note says why a request went back to waiting.
  state: "" | "PullRequestOpen" | "PullRequestUnknown"; note: string;
};
export type PullRequest = { number: number; url: string; title: string; branch: string; author?: string };
// A change in GitOps mode answers 202 with the pull request it opened; nothing changed yet.
export type Proposed = { pullRequest: PullRequest };
export type ChangeStep = { name: string; state: "done" | "waiting" | "failed" | "skipped"; detail?: string };
export type Change = PullRequest & { merged: boolean; closed: boolean; verb: string; kind: string; name: string; steps: ChangeStep[] };
export const isProposed = (v: unknown): v is Proposed => !!v && typeof v === "object" && "pullRequest" in v;
export type Config = {
  mode: "direct" | "gitops"; namespace?: string; repository?: string; branch?: string;
  // The access mode as the controller published it; pending is a change agentgateway has not applied (admins only).
  accessMode: "enforce" | "audit"; modeSince?: string; rulesVersion?: string; rulesSince?: string;
  pending?: { mode: string; rulesVersion: string; step: string; error?: string; objects?: string[] };
};
export type RequestInput = { serverName: string; displayName: string; image: string; url: string; connection: string; transport: string; reason: string };
export type MyAccess = {
  groups: string[]; models: string[];
  servers: { server: string; allTools: boolean; tools: string[]; rules: string[] }[];
};

export async function getJSON<T>(url: string): Promise<T> {
  const r = await fetch(url, { credentials: "same-origin" });
  if (r.status === 401) { window.location.assign("/login"); throw new Error("signed out"); }
  if (!r.ok) throw new Error((await r.json().catch(() => null))?.error ?? `${url} returned ${r.status}`);
  return r.json();
}

export async function sendJSON(method: string, url: string, body?: unknown) {
  const r = await fetch(url, {
    method, credentials: "same-origin",
    headers: body === undefined ? {} : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!r.ok) throw new Error((await r.json().catch(() => null))?.error ?? `${method} ${url} returned ${r.status}`);
  return r.status === 204 ? null : r.json();
}

export const useModels = () => useQuery({ queryKey: ["models"], queryFn: () => getJSON<ModelView[]>("/api/models") });
export const useRules = () => useQuery({ queryKey: ["rules"], queryFn: () => getJSON<RuleView[]>("/api/access-rules") });
export type GroupView = { name: string; source: "rules" | "sign-in" | "both" };
export const useGroups = (enabled = true) => useQuery({ queryKey: ["groups"], enabled, queryFn: () => getJSON<GroupView[]>("/api/groups") });
export const useRule = (name?: string) =>
  useQuery({ queryKey: ["rule", name], enabled: !!name, queryFn: () => getJSON<RuleDetail>(`/api/access-rules/${encodeURIComponent(name!)}`) });
export const useServers = () => useQuery({ queryKey: ["servers"], queryFn: () => getJSON<ServerView[]>("/api/servers") });
export const useRequests = () => useQuery({ queryKey: ["server-requests"], queryFn: () => getJSON<RequestView[]>("/api/server-requests") });
export const useMyAccess = () => useQuery({ queryKey: ["my-access"], queryFn: () => getJSON<MyAccess>("/api/me/access") });
export const useConfig = () => useQuery({ queryKey: ["config"], staleTime: 60_000, refetchInterval: 60_000, queryFn: () => getJSON<Config>("/api/config") });
export const useChanges = (enabled: boolean) =>
  useQuery({ queryKey: ["changes"], enabled, queryFn: () => getJSON<PullRequest[]>("/api/changes") });
export type ConnectionView = {
  name: string; displayName: string; state: "not-connected" | "connected" | "broken"; error?: string;
  connectedAt?: string; checkedAt?: string; revocable: boolean; connectURL: string; servers: string[];
};
export const useConnections = () => useQuery({ queryKey: ["connections"], queryFn: () => getJSON<ConnectionView[]>("/api/connections") });

export type Placeholder = { name: string; label: string; pattern: string; example: string };
export type PresetView = { id: string; version: number; displayName: string; category: string; serverHost: string; serverURL: string;
  scopes: string[]; setup: string; placeholders: Placeholder[]; clientIDLabel: string; clientSecretLabel: string;
  requires?: string; preview: boolean; revokes: boolean; testedOn?: string; docs: string };
export type PresetOverrides = { serverURL?: string; scopes?: string[] };
export type PresetAddInput = { operation: string; preset: string; version: number; name: string; displayName: string;
  clientID: string; clientSecret: string; values: Record<string, string>; overrides?: PresetOverrides };
export type Unfinished = { operation: string; app: string; missing: string[]; pullRequest?: PullRequest };
export type EffectiveAccess = { allSignedIn: boolean; groups: string[]; allTools: boolean; tools: Record<string, string[]> };
