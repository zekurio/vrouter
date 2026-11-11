import { useId, useState } from "react";
import { Info } from "lucide-react";
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
// share columns and fill them in order from the first, so every account row
// starts at the same x.
export function QuotaWindows({ windows }: { windows: QuotaWindow[] }) {
  const now = Date.now();
  return (
    <div className="grid grid-cols-[repeat(auto-fill,minmax(min(260px,100%),1fr))] gap-x-7 gap-y-5">
      {windows.map((w) => (
        <QuotaCell key={w.id} window={w} now={now} />
      ))}
    </div>
  );
}

// The mark for even spending, by which side of it usage is.
const marks = { deficit: "bg-danger", reserve: "bg-green" };

function QuotaCell({ window: w, now }: { window: QuotaWindow; now: number }) {
  const p = pace(w, now);
  // Within 2 points of even spending counts as on pace, with no mark.
  const off =
    p && Math.abs(p.delta) > 2 ? (p.delta > 0 ? "deficit" : "reserve") : "";
  const resetAt = w.resetAt ?? "";
  const reset = resetAt ? new Date(resetAt).getTime() - now : 0;
  return (
    <div>
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

// An account row's windows. Same height as the identity block beside them, so
// rows with one window or none keep the height of the rest. Phones give them
// the whole row, under the switch too.
export function AccountWindows({
  windows,
  failed,
  off,
}: {
  windows: QuotaWindow[];
  // The last usage read failed. QuotaIssue beside the name says why.
  failed: boolean;
  // Dimmed with the rest of a disabled account's row.
  off: boolean;
}) {
  return (
    <div
      className={cx(
        "grid min-h-11.5 content-center max-md:col-[2/4] max-xs:col-span-full",
        off && "opacity-50",
      )}
    >
      {windows.length > 0 ? (
        <QuotaWindows windows={windows} />
      ) : (
        <p className="text-[12px] leading-normal text-muted">
          {failed ? "Usage unavailable" : "Allowance not reported"}
        </p>
      )}
    </div>
  );
}

// The account fields QuotaIssue reads.
type QuotaReport = {
  windows?: QuotaWindow[];
  quotaError?: string;
  quotaUpdatedAt?: string;
  quotaRetryAt?: string;
};

// How old the windows on show are, and when vrouter next asks the provider.
function issueTimes(a: QuotaReport, now: number) {
  const updated = a.quotaUpdatedAt ?? "";
  const retry = a.quotaRetryAt ?? "";
  const age =
    updated && (a.windows ?? []).length > 0
      ? now - new Date(updated).getTime()
      : 0;
  const wait = retry ? new Date(retry).getTime() - now : 0;
  return [
    age > 0 && `Values from ${span(age)} ago`,
    wait > 0 && `Retrying in ${span(wait)}`,
  ]
    .filter(Boolean)
    .join(" · ");
}

// An amber mark beside an account whose last usage read failed, and nothing
// for the rest. Hovering or focusing it shows the error. The card hangs from
// the nearest positioned ancestor, so callers put the mark in a relative line
// and the card lines up under that line.
export function QuotaIssue({ account }: { account: QuotaReport }) {
  const error = account.quotaError ?? "";
  return error ? <IssueMark account={account} error={error} /> : null;
}

function IssueMark({
  account,
  error,
}: {
  account: QuotaReport;
  error: string;
}) {
  const [hover, setHover] = useState(false);
  const [focus, setFocus] = useState(false);
  const id = useId();
  const shown = hover || focus;
  // With windows, they are the last good read.
  const title =
    (account.windows ?? []).length > 0
      ? "Couldn't refresh usage"
      : "Couldn't read usage";
  const times = shown ? issueTimes(account, Date.now()) : "";
  return (
    // The card is inside the hover area, so the pointer can move onto it.
    <span
      className="inline-flex shrink-0"
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
    >
      <button
        type="button"
        // The mark is small, so an unseen margin around it takes taps too.
        className="relative inline-flex size-4 cursor-default items-center justify-center rounded-full text-amber before:absolute before:-inset-3"
        aria-label={title}
        aria-describedby={shown ? id : undefined}
        onFocus={() => setFocus(true)}
        onBlur={() => setFocus(false)}
        // Safari and touch browsers do not focus a button on click.
        onClick={(e) => e.currentTarget.focus()}
        onKeyDown={(e) => {
          if (e.key !== "Escape") return;
          setHover(false);
          setFocus(false);
        }}
      >
        <Info size={14} strokeWidth={2.25} />
      </button>
      {shown && (
        <span
          className="absolute top-full left-0 z-20 pt-1.5 text-left"
          role="tooltip"
          id={id}
        >
          <span className="grid w-max max-w-[min(300px,calc(100vw-48px))] gap-1 rounded-[10px] border border-line bg-control px-3 py-2.5 text-[12px] leading-normal font-normal shadow-[0_10px_30px_#0006] light:shadow-[0_10px_30px_#0000001f]">
            <strong className="flex items-center gap-1.5 font-[550] text-text">
              <Info size={13} strokeWidth={2.25} className="text-amber" />
              {title}
            </strong>
            <span className="text-subtle wrap-anywhere">
              <Private>{error}</Private>
            </span>
            {times && <span className="text-muted tabular-nums">{times}</span>}
          </span>
        </span>
      )}
    </span>
  );
}
