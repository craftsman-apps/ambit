import { useId, useRef, useState } from "react";
import type { GroupView } from "@/api";

// Same pattern as the AccessRule CRD and the console's server-side check.
const GROUP_RE = /^[A-Za-z0-9][A-Za-z0-9._@/-]*$/;
const MAX_SHOWN = 50;

// A multi-value field: type to filter the known groups, Enter or a click adds a chip. A name that is not known
// can be added too (a group may not have signed in yet); it must match the CRD pattern.
export function GroupField({ value, onChange, known }: { value: string[]; onChange: (v: string[]) => void; known: GroupView[] }) {
  const id = useId();
  const listId = `${id}-list`;
  const errId = `${id}-error`;
  const input = useRef<HTMLInputElement>(null);
  const [text, setText] = useState("");
  const [open, setOpen] = useState(false);
  const [moved, setMoved] = useState<number | null>(null); // set by Up/Down; reset when the text changes
  const [error, setError] = useState("");
  const [note, setNote] = useState("");

  const q = text.trim().toLowerCase();
  const matches = known.map((g) => g.name).filter((n) => !value.includes(n) && n.toLowerCase().includes(q)).slice(0, MAX_SHOWN);
  const typed = text.trim();
  // With no match, one row offers to add the typed text.
  const rows = matches.length ? matches : typed ? [typed] : [];
  const addRow = matches.length === 0 && !!typed;
  const exact = matches.findIndex((n) => n.toLowerCase() === q);
  const active = rows.length === 0 ? -1 : moved !== null ? Math.min(moved, rows.length - 1) : q ? (exact >= 0 ? exact : 0) : -1;
  const showList = open && rows.length > 0;

  const add = (name: string) => {
    name = name.trim();
    if (!name) return;
    if (value.includes(name)) { setText(""); setError(""); setNote(`${name} is already added`); return; }
    if (!GROUP_RE.test(name)) {
      setError(`"${name}" is not a valid group name: use letters, digits and . _ @ / -, starting with a letter or digit`);
      return;
    }
    onChange([...value, name]);
    setText(""); setMoved(null); setError(""); setNote(`Added ${name}`);
  };
  const remove = (name: string) => {
    onChange(value.filter((x) => x !== name));
    setNote(`Removed ${name}`);
    input.current?.focus();
  };
  const edit = (t: string) => {
    setText(t); setMoved(null); setError(""); setOpen(true);
    const n = known.filter((g) => !value.includes(g.name) && g.name.toLowerCase().includes(t.trim().toLowerCase())).length;
    setNote(t.trim() ? `${n} matching ${n === 1 ? "group" : "groups"}` : "");
  };
  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      if (!showList) { setOpen(true); return; }
      const step = e.key === "ArrowDown" ? 1 : -1;
      setMoved((active + step + rows.length) % rows.length);
    } else if (e.key === "Enter") {
      if (active >= 0 && showList) { e.preventDefault(); add(rows[active]); }
      else if (typed) { e.preventDefault(); add(typed); } // never submit the form while text is waiting to be added
    } else if (e.key === "Escape") {
      if (showList) { e.preventDefault(); e.stopPropagation(); setOpen(false); }
    } else if (e.key === "Backspace" && text === "" && value.length) {
      remove(value[value.length - 1]);
    }
  };

  return (
    <div className="space-y-1">
      <div className="relative">
        <div className="flex flex-wrap items-center gap-1.5 rounded-md border border-line bg-surface px-2 py-1.5 focus-within:border-accent" onClick={() => input.current?.focus()}>
          {value.map((g) => (
            <span key={g} className="inline-flex items-center gap-1 rounded bg-selected px-1.5 py-0.5 font-mono text-small" data-testid={`group-chip-${g}`}>
              {g}
              <button type="button" aria-label={`Remove ${g}`} className="px-0.5 text-muted hover:text-bad" onClick={(e) => { e.stopPropagation(); remove(g); }}>×</button>
            </span>
          ))}
          <input ref={input} id="rule-groups" role="combobox" aria-label="Group names" aria-autocomplete="list" aria-expanded={showList}
            aria-controls={listId} aria-activedescendant={showList && active >= 0 ? `${id}-opt-${active}` : undefined}
            aria-invalid={!!error} aria-describedby={error ? errId : undefined}
            className="min-w-32 flex-1 bg-transparent py-0.5 font-mono text-body outline-none" value={text} placeholder={value.length ? "" : "Type to search, e.g. ml-research"}
            autoComplete="off" onChange={(e) => edit(e.target.value)} onKeyDown={onKeyDown} onFocus={() => setOpen(true)} onBlur={() => setOpen(false)} />
        </div>
        <ul id={listId} role="listbox" aria-label="Known groups" hidden={!showList}
          className="absolute z-10 mt-1 max-h-60 w-full overflow-auto rounded-md border border-line bg-surface py-1 shadow-md">
          {showList && rows.map((n, i) => (
            <li key={n} id={`${id}-opt-${i}`} role="option" aria-selected={i === active}
              className={`cursor-pointer px-2.5 py-1 font-mono text-body ${i === active ? "bg-selected" : ""}`}
              onMouseDown={(e) => e.preventDefault()} onClick={() => add(n)}>
              {addRow ? `Add '${n}'` : n}
              {!addRow && <span className="ml-2 text-small text-muted">{known.find((g) => g.name === n)?.source}</span>}
            </li>
          ))}
        </ul>
      </div>
      {error && <p id={errId} className="text-small text-bad">{error}</p>}
      <p className="text-small text-muted" data-testid="groups-help">
        Groups come from your identity provider. Ambit does not create them. The bootstrap admin has no groups; to test locally, use <strong>Everyone signed in</strong>.{" "}
        <a className="underline" href="https://ambitproject.dev/docs/install#groups-and-console-roles" target="_blank" rel="noreferrer">Groups and console roles</a>
      </p>
      <div aria-live="polite" className="sr-only">{note}</div>
    </div>
  );
}
