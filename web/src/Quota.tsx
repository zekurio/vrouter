export type QuotaWindow = {
  id: string;
  label: string;
  remaining: number;
  resetAt?: string;
  // Window length. The pace needs it together with resetAt.
  seconds?: number;
};

// Pace as CodexBar works it out: usage so far against the share of the window
// that has passed, and when the quota runs out if usage keeps that rate. Null
// without a length and a reset time, for an empty window, and in the first 3%
// of a window, which is too little to go on.
function pace(window: QuotaWindow, now: number) {
  const { seconds, resetAt } = window;
  if (
    seconds === undefined ||
    seconds === 0 ||
    resetAt === undefined ||
    resetAt === "" ||
    window.remaining <= 0
  )
    return null;
  const length = seconds * 1000;
  const left = new Date(resetAt).getTime() - now;
  if (!(left > 0 && left <= length)) return null;
  const elapsed = length - left;
  if (elapsed < length * 0.03) return null;
  const used = 100 - window.remaining;
  // Infinite when nothing is used yet.
  const emptyIn = (window.remaining / used) * elapsed;
  return {
    // Remaining with even spending, where the bar marks the pace.
    even: (left / length) * 100,
    // Points used beyond even spending. Negative is quota in reserve.
    delta: used - (elapsed / length) * 100,
    emptyIn: emptyIn < left ? emptyIn : null,
  };
}

// The two largest units, rounded up to the minute: "6d 6h", "4h 29m" or "12m".
function span(ms: number) {
  const minutes = Math.max(1, Math.ceil(ms / 60000));
  const parts: [number, string][] = [
    [Math.floor(minutes / 1440), "d"],
    [Math.floor(minutes / 60) % 24, "h"],
    [minutes % 60, "m"],
  ];
  return parts
    .filter(([n]) => n > 0)
    .slice(0, 2)
    .map(([n, unit]) => `${n}${unit}`)
    .join(" ");
}

// Each window as the amount left and the reset, a bar, and the pace.
export function QuotaWindows({ windows }: { windows: QuotaWindow[] }) {
  const now = Date.now();
  return (
    <div className="quota-list">
      {windows.map((w) => {
        const p = pace(w, now);
        // Within 2 points of even spending counts as on pace, with no mark.
        const off =
          p && Math.abs(p.delta) > 2
            ? p.delta > 0
              ? "deficit"
              : "reserve"
            : "";
        const resetAt = w.resetAt ?? "";
        const reset = resetAt ? new Date(resetAt).getTime() - now : 0;
        return (
          <div
            className={`quota-window ${off && `is-${off}`}`}
            key={w.id}
            data-window={w.id}
          >
            <div className="quota-head">
              <span title={w.label}>{w.label}</span>
              <strong>{w.remaining}% left</strong>
              {resetAt !== "" && (
                <time
                  dateTime={resetAt}
                  title={new Date(resetAt).toLocaleString()}
                >
                  {reset > 0 ? `Resets in ${span(reset)}` : "Reset due"}
                </time>
              )}
            </div>
            <div className="progress">
              <span style={{ width: `${w.remaining}%` }} />
              {p && off && <i style={{ left: `${p.even}%` }} />}
            </div>
            {p && (
              <p className="quota-meta">
                {`${off ? `${Math.round(Math.abs(p.delta))}% in ${off}` : "On pace"} · ${
                  p.emptyIn === null
                    ? "Lasts until reset"
                    : `Runs out in ${span(p.emptyIn)}`
                }`}
              </p>
            )}
          </div>
        );
      })}
    </div>
  );
}
