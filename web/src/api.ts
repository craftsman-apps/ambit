import { useQuery } from "@tanstack/react-query";

export type Me = {
  email: string;
  name: string;
  role: "admin" | "approver" | "viewer";
  groups?: string[];
  bootstrap: boolean;
};

export type LoginOptions = { bootstrap: boolean; sso: { id: string; name: string }[] };

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

export type ModelView = {
  name: string; displayName: string; model: string; provider: string;
  allowedGroups: string[]; allSignedIn: boolean; ready: string; message: string;
};
export type ServerAccess = { name: string; tools?: string[] };
export type RuleView = {
  name: string; groups: string[]; allSignedIn: boolean; models: string[]; servers: ServerAccess[];
  ready: string; reason: string; message: string; gitManaged: boolean; createdBy: string;
};
export type RuleDetail = RuleView & {
  effective: { model: string; modelName: string; cel: string }[];
  effectiveServers: { server: string; cel: string }[];
};
export type RuleInput = { name?: string; groups: string[]; allSignedIn: boolean; models: string[]; servers: ServerAccess[] };

export type Tool = { name: string; description?: string };
export type ServerView = {
  name: string; displayName: string; image: string; transport: string; ready: string; message: string;
  tools: Tool[]; requestedBy: string; approvedBy: string; gitManaged: boolean; rules: string[]; changedTools: string[];
};
export type RequestView = {
  name: string; serverName: string; displayName: string; image: string; transport: string; reason: string;
  requestedBy: string; requestedAt: string; decision: "" | "Approved" | "Denied"; decidedBy: string; decidedAt: string | null; comment: string;
  pullRequest: string;
};
export type PullRequest = { number: number; url: string; title: string; branch: string; author?: string };
// A change in GitOps mode answers 202 with the pull request it opened; nothing changed yet.
export type Proposed = { pullRequest: PullRequest };
export const isProposed = (v: unknown): v is Proposed => !!v && typeof v === "object" && "pullRequest" in v;
export type Config = { mode: "direct" | "gitops"; repository?: string; branch?: string };
export type RequestInput = { serverName: string; displayName: string; image: string; transport: string; reason: string };
export type MyAccess = {
  groups: string[]; models: string[];
  servers: { server: string; allTools: boolean; tools: string[]; rules: string[] }[];
};

async function getJSON<T>(url: string): Promise<T> {
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
export const useRule = (name?: string) =>
  useQuery({ queryKey: ["rule", name], enabled: !!name, queryFn: () => getJSON<RuleDetail>(`/api/access-rules/${encodeURIComponent(name!)}`) });
export const useServers = () => useQuery({ queryKey: ["servers"], queryFn: () => getJSON<ServerView[]>("/api/servers") });
export const useRequests = () => useQuery({ queryKey: ["server-requests"], queryFn: () => getJSON<RequestView[]>("/api/server-requests") });
export const useMyAccess = () => useQuery({ queryKey: ["my-access"], queryFn: () => getJSON<MyAccess>("/api/me/access") });
export const useConfig = () => useQuery({ queryKey: ["config"], staleTime: Infinity, queryFn: () => getJSON<Config>("/api/config") });
export const useChanges = (enabled: boolean) =>
  useQuery({ queryKey: ["changes"], enabled, queryFn: () => getJSON<PullRequest[]>("/api/changes") });
export type ConnectionView = {
  name: string; displayName: string; state: "not-connected" | "connected" | "broken"; error?: string;
  connectedAt?: string; checkedAt?: string; revocable: boolean; connectURL: string; servers: string[];
};
export const useConnections = () => useQuery({ queryKey: ["connections"], queryFn: () => getJSON<ConnectionView[]>("/api/connections") });
