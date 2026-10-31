import type { Account } from "./AccountsPage";
import { PoolChart } from "./PoolChart";
import {
  bases,
  basisLabel,
  type Basis,
  type PoolForecast,
} from "./poolForecast";
import { pageNote, pageToolbar } from "./pageStyles";
import { span } from "./Quota";
import {
  ProviderBrand as Brand,
  providerLabel,
  providerStyle,
} from "./ProviderBrand";
import { dayLabel } from "./usage";
import { cx, panelFrame, tab, tabs } from "./ui";

// The panel padding, with a little more below. Account cards share it.
export const card = `${panelFrame} px-5.5 pt-5 pb-5.5 max-sm:p-4.5`;

// What the pool projections take as the coming rate of spending. It scopes
// every pool, so it sits above them.
export function ProjectionBasis({
  basis,
  choose,
}: {
  basis: Basis;
  choose: (basis: Basis) => void;
}) {
  return (
    <div className={pageToolbar}>
      <span className={pageNote}>Project from</span>
      <div className={tabs()} role="group" aria-label="Project from">
        {bases.map((b) => (
          <button
            key={b}
            aria-pressed={basis === b}
            className={tab(basis === b)}
            onClick={() => choose(b)}
          >
            {basisLabel[b]}
          </button>
        ))}
      </div>
    </div>
  );
}

export function PoolCard({
  provider: p,
  accounts,
  forecast: f,
  basis,
  filter,
  toggle,
}: {
  provider: string;
  accounts: Account[];
  // Null when no account reports its allowance.
  forecast: PoolForecast | null;
  basis: Basis;
  filter: string;
  // Shows only this pool, or every pool again. Unset with a single pool.
  toggle: (() => void) | undefined;
}) {
  const known = accounts.filter((a) => a.remaining !== null);
  const remaining = known.reduce((sum, a) => sum + (a.remaining ?? 0), 0);
  const stats = poolStats(accounts, f);
  return (
    <article
      className={cx(
        card,
        // A column, so the chart sits at the bottom and lines up with the
        // pool beside it.
        "relative flex flex-col transition-opacity duration-150 ease-[ease]",
        filter &&
          filter !== p &&
          "opacity-40 focus-within:opacity-75 hover:opacity-75",
      )}
      style={providerStyle(p)}
    >
      <div className="flex items-center gap-2.5">
        <Brand provider={p} />
        <h2 className="text-[15px] font-semibold">
          {toggle ? (
            // The pool name filters the account list. Its hit area covers
            // the panel.
            <button
              className="after:absolute after:inset-0 after:rounded-[inherit]"
              aria-pressed={filter === p}
              title={
                filter === p
                  ? "Show all pools"
                  : `Show only ${providerLabel(p)} accounts`
              }
              onClick={toggle}
            >
              {providerLabel(p)}
            </button>
          ) : (
            providerLabel(p)
          )}
        </h2>
        <span className="text-[13px] text-muted">
          {accounts.length} {accounts.length === 1 ? "account" : "accounts"}
        </span>
      </div>
      <div className="mt-5.5 flex items-baseline gap-2.5 leading-none">
        {known.length === accounts.length ? (
          <>
            <span className="text-[46px] font-semibold tracking-[-1.6px] max-sm:text-[40px]">
              {Math.round(remaining)}
              <small className="ml-0.5 text-[24px] font-medium tracking-[-0.5px] text-muted">
                %
              </small>
            </span>
            {accounts.length > 1 && (
              <span className="text-[14px] text-muted">
                of {accounts.length * 100}%
              </span>
            )}
          </>
        ) : (
          <span className="flex h-11.5 items-center text-[24px] font-semibold tracking-[-0.5px] text-muted">
            Not reported
          </span>
        )}
      </div>
      <p className="mt-2.5 text-[13px] text-muted">
        {accounts.every((a) => a.window === "Weekly window")
          ? "Weekly remaining"
          : "Allowance remaining"}
      </p>
      {stats.length > 0 && (
        // Columns of one width, so pools side by side wrap their stats
        // alike and line up.
        <dl className="mt-5 grid grid-cols-[repeat(auto-fill,minmax(7.5rem,1fr))] gap-x-6 gap-y-3.5">
          {stats.map((s) => (
            <div key={s.label}>
              <dt className="text-[13px] whitespace-nowrap text-muted">
                {s.label}
              </dt>
              <dd className="mt-1.25 text-[15px] font-[550] whitespace-nowrap">
                {s.value}
              </dd>
            </div>
          ))}
        </dl>
      )}
      {f && <PoolChart forecast={f} label={providerLabel(p)} basis={basis} />}
    </article>
  );
}

// When the projected pool runs dry, if it does before the chart ends.
function runsOut(f: PoolForecast) {
  if (f.empty === null) return `Not by ${dayLabel(f.to)}`;
  return f.empty > f.now ? `In ${span(f.empty - f.now)}` : "Now";
}

// Percent of one account's allowance per hour.
function rate(value: number) {
  if (value > 0 && value < 0.05) return "<0.1%/h";
  return `${value.toFixed(1)}%/h`;
}
const burnLabel: Record<Basis, string> = {
  "3h": "Burn, last 3h",
  day: "Burn, last day",
  window: "Burn since reset",
};

// The pool's other quota windows, summed like the headline figure, when the
// projection runs dry, when the headline window next resets, and how fast
// the pool is spent.
function poolStats(accounts: Account[], f: PoolForecast | null) {
  const windows = new Map<string, { label: string; left: number }>();
  let reset = Infinity;
  for (const a of accounts) {
    for (const w of a.windows ?? []) {
      if (w.label === a.window) {
        const at = new Date(w.resetAt ?? "").getTime();
        if (at < reset) reset = at;
        continue;
      }
      const pooled = windows.get(w.id) ?? { label: w.label, left: 0 };
      pooled.left += w.remaining;
      windows.set(w.id, pooled);
    }
  }
  const stats = [...windows.values()].map((w) => ({
    label: w.label,
    value: `${Math.round(w.left)}% left`,
  }));
  if (f) stats.push({ label: "Runs out", value: runsOut(f) });
  const until = reset - Date.now();
  if (Number.isFinite(until))
    stats.push({
      label: "Next reset",
      value: until > 0 ? `In ${span(until)}` : "Due",
    });
  if (f) stats.push({ label: burnLabel[f.basis], value: rate(f.rate) });
  return stats;
}
