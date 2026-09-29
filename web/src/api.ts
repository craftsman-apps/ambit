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
export type RuleView = {
  name: string; groups: string[]; allSignedIn: boolean; models: string[];
  ready: string; reason: string; message: string; gitManaged: boolean; createdBy: string;
};
export type RuleDetail = RuleView & { effective: { model: string; modelName: string; cel: string }[] };
export type RuleInput = { name?: string; groups: string[]; allSignedIn: boolean; models: string[] };

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
