import { useQuery } from "@tanstack/react-query";

export type Me = {
  email: string;
  name: string;
  role: "admin" | "approver" | "viewer";
  groups?: string[];
  bootstrap: boolean;
};

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
