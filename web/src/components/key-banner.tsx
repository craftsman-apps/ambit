import { day, useKeys } from "@/routes/keys";

const WEEK = 7 * 864e5;

// One line when any of the person's own keys stops within 7 days.
export function KeyBanner() {
  const soon = useKeys(false).data?.keys.find((k) => {
    const left = new Date(k.until).getTime() - Date.now();
    return left > 0 && left <= WEEK;
  });
  if (!soon) return null;
  return (
    <p className="border-b border-line bg-warn-soft px-4 py-1.5" role="status" data-testid="key-banner">
      Your key ‘{soon.name}’ stops on {day(soon.until)}. Sign in again or create a new key.
    </p>
  );
}
