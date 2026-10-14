import { useState } from "react";
import { cn } from "@/lib/utils";

type Choice = "system" | "light" | "dark";
const KEY = "ambit-theme";

function read(): Choice {
  try {
    const v = localStorage.getItem(KEY);
    return v === "light" || v === "dark" ? v : "system";
  } catch {
    return "system";
  }
}

// System follows the operating system; Light and Dark override it in this browser only.
export function ThemeSwitch() {
  const [choice, setChoice] = useState<Choice>(read);
  const pick = (c: Choice) => {
    setChoice(c);
    try {
      if (c === "system") localStorage.removeItem(KEY);
      else localStorage.setItem(KEY, c);
    } catch {
      /* storage blocked: the choice lasts until reload */
    }
    if (c === "system") delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = c;
  };
  return (
    <div role="group" aria-label="Color mode" className="inline-flex rounded-md border border-line p-0.5 text-small">
      {(["system", "light", "dark"] as const).map((c) => (
        <button
          key={c}
          type="button"
          aria-pressed={choice === c}
          onClick={() => pick(c)}
          className={cn(
            "rounded-[3px] px-2 py-1 capitalize text-muted focus-visible:outline-2 focus-visible:outline-accent",
            choice === c && "bg-selected text-ink",
          )}
        >
          {c}
        </button>
      ))}
    </div>
  );
}
