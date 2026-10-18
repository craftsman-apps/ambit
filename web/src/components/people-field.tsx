import { useId, useRef, useState } from "react";

// Same pattern as the AccessRule CRD and the console's server-side check; the value is lowercased and trimmed first.
const EMAIL_RE = /^[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}$/;
const MAX = 50;

// Email addresses as chips: Enter, a comma or a space adds the typed address.
export function PeopleField({ value, onChange }: { value: string[]; onChange: (v: string[]) => void }) {
  const id = useId();
  const errId = `${id}-error`;
  const input = useRef<HTMLInputElement>(null);
  const [text, setText] = useState("");
  const [error, setError] = useState("");
  const [note, setNote] = useState("");

  const add = (raw: string) => {
    const email = raw.trim().toLowerCase();
    if (!email) return true;
    if (value.includes(email)) { setText(""); setError(""); setNote(`${email} is already added`); return true; }
    if (!EMAIL_RE.test(email) || email.length > 254) {
      setError(`"${raw.trim()}" is not an email address: use letters, digits and . _ % + - only`);
      return false;
    }
    if (value.length >= MAX) { setError(`At most ${MAX} people; use a group for more`); return false; }
    onChange([...value, email]);
    setText(""); setError(""); setNote(`Added ${email}`);
    return true;
  };
  const remove = (email: string) => {
    onChange(value.filter((x) => x !== email));
    setNote(`Removed ${email}`);
    input.current?.focus();
  };
  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if ((e.key === "Enter" || e.key === "," || e.key === " ") && text.trim()) {
      e.preventDefault(); // never submit the form while text is waiting to be added
      add(text);
    } else if (e.key === "Enter") {
      // an empty box: let Enter submit the form
    } else if (e.key === "Backspace" && text === "" && value.length) {
      remove(value[value.length - 1]);
    }
  };

  return (
    <div className="space-y-1">
      <div className="flex flex-wrap items-center gap-1.5 rounded-md border border-line bg-surface px-2 py-1.5 focus-within:border-accent" onClick={() => input.current?.focus()}>
        {value.map((p) => (
          <span key={p} className="inline-flex items-center gap-1 rounded bg-selected px-1.5 py-0.5 font-mono text-small" data-testid={`person-chip-${p}`}>
            {p}
            <button type="button" aria-label={`Remove ${p}`} className="px-0.5 text-muted hover:text-bad" onClick={(e) => { e.stopPropagation(); remove(p); }}>×</button>
          </span>
        ))}
        <input ref={input} id="rule-users" type="text" inputMode="email" aria-label="Email addresses" aria-invalid={!!error} aria-describedby={error ? errId : undefined}
          className="min-w-32 flex-1 bg-transparent py-0.5 font-mono text-body outline-none" value={text} placeholder={value.length ? "" : "alice@acme.com"}
          autoComplete="off" onChange={(e) => { setText(e.target.value); setError(""); }} onKeyDown={onKeyDown} onBlur={() => add(text)} />
      </div>
      {error && <p id={errId} className="text-small text-bad">{error}</p>}
      <p className="text-small text-muted" data-testid="people-help">Prefer groups; use people for exceptions.</p>
      <div aria-live="polite" className="sr-only">{note}</div>
    </div>
  );
}
