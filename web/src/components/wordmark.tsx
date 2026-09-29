export function Wordmark() {
  return (
    <div className="flex items-center gap-2.5">
      <img src="/favicon.svg" alt="" className="size-7" />
      <div className="leading-tight">
        <div className="text-base font-semibold tracking-tight">Ambit</div>
        <div className="font-mono text-[10.5px] uppercase tracking-widest text-muted">Gateway console</div>
      </div>
    </div>
  );
}
