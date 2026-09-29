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
