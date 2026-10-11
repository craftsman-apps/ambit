// The Ambit mark (docs/assets/brand): an open ring with one exit. The ring takes the text color and the exit the
// accent text color, so it follows the light and dark themes.
export function Mark({ className = "size-6" }: { className?: string }) {
  return (
    <svg viewBox="0 0 32 32" className={className} aria-hidden="true">
      <path d="M25.53 21.50A11 11 0 1 1 25.53 10.50" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
      <path d="M16 16H30" fill="none" stroke="var(--color-accent-text)" strokeWidth="3" strokeLinecap="round" />
      <circle cx="16" cy="16" r="3.5" fill="var(--color-accent-text)" />
    </svg>
  );
}

export function Wordmark() {
  return (
    <div className="flex items-center gap-2.5">
      <Mark />
      <div className="leading-tight">
        <div className="text-[15px] font-semibold tracking-tight">Ambit</div>
        <div className="font-mono text-[10.5px] uppercase tracking-widest text-muted">Gateway console</div>
      </div>
    </div>
  );
}
