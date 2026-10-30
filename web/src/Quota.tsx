import { Private } from "./Privacy";
import { cx } from "./ui";

// A provider-tinted bar, and its filled part.
const track =
  "overflow-hidden rounded-[3px] bg-[color-mix(in_srgb,var(--color-provider)_16%,var(--color-surface))]";
const fill = "block h-full rounded-[3px] bg-provider";

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
export function span(ms: number) {
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

// Each window as the amount left and the reset, a bar, and the pace. Cells
// share columns, so the windows of every account row line up. With aligned,
// a weekly window without a 5-hour window keeps the second column, so it
// lines up with the weekly bars of accounts that report both.
export function QuotaWindows({
  windows,
  aligned = false,
}: {
  windows: QuotaWindow[];
  aligned?: boolean;
}) {
  const now = Date.now();
  const weeklyFirst = aligned && windows[0]?.id === "weekly";
  return (
    <div className="@container grid grid-cols-[repeat(auto-fill,minmax(min(260px,100%),1fr))] gap-x-7 gap-y-5">
      {windows.map((w, i) => (
        <QuotaCell
          key={w.id}
          window={w}
          now={now}
          secondColumn={weeklyFirst && i === 0}
        />
      ))}
    </div>
  );
}

// The mark for even spending, by which side of it usage is.
const marks = { deficit: "bg-danger", reserve: "bg-green" };

function QuotaCell({
  window: w,
  now,
  secondColumn,
}: {
  window: QuotaWindow;
  now: number;
  secondColumn: boolean;
}) {
  const p = pace(w, now);
  // Within 2 points of even spending counts as on pace, with no mark.
  const off =
    p && Math.abs(p.delta) > 2 ? (p.delta > 0 ? "deficit" : "reserve") : "";
  const resetAt = w.resetAt ?? "";
  const reset = resetAt ? new Date(resetAt).getTime() - now : 0;
  return (
    <div className={secondColumn ? "@min-[548px]:col-start-2" : ""}>
      {/* One line, so the bars of a grid row line up. A long label gives way
          first. */}
      <div className="flex items-baseline gap-1.5 text-[12px] whitespace-nowrap text-muted">
        <span className="min-w-0 truncate" title={w.label}>
          {w.label}
        </span>
        <strong className="font-[550] text-text tabular-nums">
          {w.remaining}% left
        </strong>
        {resetAt !== "" && (
          <time
            className="ml-auto pl-1.5"
            dateTime={resetAt}
            title={new Date(resetAt).toLocaleString()}
          >
            {reset > 0 ? `Resets in ${span(reset)}` : "Reset due"}
          </time>
        )}
      </div>
      <div className={cx(track, "relative mt-2 h-1.5")}>
        <span className={fill} style={{ width: `${w.remaining}%` }} />
        {/* Even spending, marked when usage is off it. The panel ring cuts it
            out of the bar. */}
        {p && off && (
          <i
            className={cx(
              "absolute top-0 bottom-0 -ml-px w-0.5 shadow-[0_0_0_1.5px_var(--color-surface)]",
              marks[off],
            )}
            style={{ left: `${p.even}%` }}
          />
        )}
      </div>
      {p && (
        <p className="mt-2 text-[12px] leading-normal text-muted">
          {`${off ? `${Math.round(Math.abs(p.delta))}% in ${off}` : "On pace"} · ${
            p.emptyIn === null
              ? "Lasts until reset"
              : `Runs out in ${span(p.emptyIn)}`
          }`}
        </p>
      )}
    </div>
  );
}

// A line in place of an account row's windows, or under them.
const windowsNote = (error: boolean) =>
  cx(
    "text-[12px] leading-normal wrap-anywhere",
    error ? "text-danger" : "text-muted",
  );

// An account row's windows. Same height as the identity block beside them, so
// rows with one window or none keep the height of the rest. Phones give them
// the whole row, under the switch too.
export function AccountWindows({
  windows,
  quotaError,
  off,
}: {
  windows: QuotaWindow[];
  quotaError: string;
  // Dimmed with the rest of a disabled account's row.
  off: boolean;
}) {
  return (
    <div
      className={cx(
        "grid min-h-11.5 content-center gap-2 max-lg:col-[2/4] max-xs:col-span-full",
        off && "opacity-50",
      )}
    >
      {windows.length > 0 ? (
        <QuotaWindows windows={windows} aligned />
      ) : (
        <p className={windowsNote(quotaError !== "")}>
          <Private>{quotaError || "Allowance not reported"}</Private>
        </p>
      )}
      {windows.length > 0 && quotaError && (
        <p className={windowsNote(true)}>
          <Private>{quotaError}</Private>
        </p>
      )}
    </div>
  );
}
