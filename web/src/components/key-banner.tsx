import { day, useKeys } from "@/routes/keys";

// One line when any of the person's own keys stops within 7 days (the server's "stops-soon" status).
export function KeyBanner() {
  const soon = useKeys(false).data?.keys.find((k) => k.status === "stops-soon");
  if (!soon) return null;
  return (
    <p className="border-b border-line bg-warn-soft px-4 py-1.5" role="status" data-testid="key-banner">
      Your key ‘{soon.name}’ stops on {day(soon.until)}.{" "}
      {soon.stopsBecause === "inactivity"
        ? "Sign in again to keep it working."
        : `Create a new key before ${day(soon.until)}.`}
    </p>
  );
}
