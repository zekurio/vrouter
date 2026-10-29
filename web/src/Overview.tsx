import { accountLabel, accountState, type Account } from "./AccountsPage";
import { badgeRow, ScopeBadge } from "./Badge";
import { Private } from "./Privacy";
import { fill, QuotaWindows, span, track } from "./Quota";
import {
  ProviderBrand as Brand,
  providerLabel,
  providerStyle,
} from "./ProviderBrand";
import { cx, empty, panelFrame } from "./ui";

// Pools and accounts both sit in a grid of cards at least 300px wide.
const cards = "grid gap-3 max-sm:grid-cols-1";
// The panel padding, with a little more below.
const card = `${panelFrame} px-5.5 pt-5 pb-5.5 max-sm:p-4.5`;
// The error under an account's windows, as in AccountsPage.
export const quotaError =
  "mt-3.5 text-[12px] leading-[1.6] text-danger wrap-anywhere";

// The account pools, with a card for each account.
export function Overview({
  accounts,
  pool,
  setPool,
}: {
  accounts: Account[];
  // Provider whose accounts are shown alone, or "" for every pool.
  pool: string;
  setPool: (pool: string) => void;
}) {
  const providers = [...new Set(accounts.map((a) => a.provider))];
  // The pool columns double as the filter. An empty filter shows every pool.
  const filter = providers.length > 1 && providers.includes(pool) ? pool : "";
  const visibleAccounts = accounts.filter(
    (a) => !filter || a.provider === filter,
  );
  return (
    <>
      {providers.length > 0 ? (
        <section
          className={cx(
            cards,
            "grid-cols-[repeat(auto-fit,minmax(300px,1fr))]",
          )}
          aria-label="Provider pools"
        >
          {providers.map((p) => (
            <PoolColumn
              key={p}
              provider={p}
              accounts={accounts.filter((a) => a.provider === p)}
              filter={filter}
              toggle={
                providers.length > 1
                  ? () => setPool(filter === p ? "" : p)
                  : undefined
              }
            />
          ))}
        </section>
      ) : (
        <div className={cx(empty, "mt-6 mb-1")}>No accounts yet</div>
      )}
      {providers.length > 0 && (
        <section
          className={cx(
            cards,
            "mt-3 grid-cols-[repeat(auto-fill,minmax(300px,1fr))]",
          )}
        >
          {visibleAccounts.map((a) => (
            <AccountCard key={a.id} account={a} />
          ))}
        </section>
      )}
    </>
  );
}

function PoolColumn({
  provider: p,
  accounts,
  filter,
  toggle,
}: {
  provider: string;
  accounts: Account[];
  filter: string;
  // Shows only this pool, or every pool again. Unset with a single pool.
  toggle: (() => void) | undefined;
}) {
  const known = accounts.filter((a) => a.remaining !== null);
  const remaining = known.reduce((sum, a) => sum + (a.remaining ?? 0), 0);
  const stats = poolStats(accounts);
  return (
    <article
      className={cx(
        card,
        "relative transition-opacity duration-150 ease-[ease]",
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
      <div className="mt-5.5 flex flex-wrap items-end justify-between gap-x-12 gap-y-4.5">
        <div>
          <div className="flex items-baseline gap-2.5 leading-none">
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
        </div>
        {stats.length > 0 && (
          <dl className="flex flex-wrap gap-x-11 gap-y-3.5 max-sm:gap-x-7">
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
      </div>
      <div className="mt-4.5 flex gap-1">
        {accounts.map((a) => (
          <div key={a.id} className={cx(track, "h-1.5 flex-1")}>
            {a.remaining !== null && (
              <span className={fill} style={{ width: `${a.remaining}%` }} />
            )}
          </div>
        ))}
      </div>
    </article>
  );
}

// The pool's other quota windows, summed like the headline figure, and when
// the headline window next resets.
function poolStats(accounts: Account[]) {
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
  const until = reset - Date.now();
  if (Number.isFinite(until))
    stats.push({
      label: "Next reset",
      value: until > 0 ? `In ${span(until)}` : "Due",
    });
  return stats;
}

function AccountCard({ account: a }: { account: Account }) {
  return (
    <article className={card} style={providerStyle(a.provider)}>
      <div className="flex items-start justify-between gap-2.5">
        <div>
          <h3 className="flex items-center gap-1.75 text-[14px] font-semibold wrap-anywhere">
            <Private peek>{accountLabel(a)}</Private>
          </h3>
          <div className={cx(badgeRow, "mt-1.5")}>
            <span className="inline-block text-[12px] font-medium text-provider">
              {a.plan || providerLabel(a.provider)}
            </span>
            <ScopeBadge scope={a.scope} />
            {accountState(a) && (
              <span
                className={cx(
                  "text-[12px]",
                  a.status === "disabled" ? "text-muted" : "text-amber",
                )}
              >
                {accountState(a)}
              </span>
            )}
          </div>
        </div>
        <Brand provider={a.provider} small />
      </div>
      {a.windows !== undefined && a.windows.length > 0 ? (
        <div className="mt-5.5">
          <QuotaWindows windows={a.windows} />
        </div>
      ) : (
        <div className="mt-5.75 flex justify-between gap-3 text-[12px] text-muted">
          <span>{a.window}</span>
          <strong className="font-medium text-text">Unknown</strong>
        </div>
      )}
      {a.quotaError !== undefined && a.quotaError !== "" && (
        <p className={quotaError}>
          <Private>{a.quotaError}</Private>
        </p>
      )}
    </article>
  );
}
