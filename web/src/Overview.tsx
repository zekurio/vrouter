import { accountLabel, accountState, type Account } from "./AccountsPage";
import { Private } from "./Privacy";
import { QuotaWindows, span } from "./Quota";
import {
  ProviderBrand as Brand,
  providerLabel,
  providerStyle,
} from "./ProviderBrand";

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
        <section className="provider-summary" aria-label="Provider pools">
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
        <div className="empty">No accounts yet</div>
      )}
      {providers.length > 0 && (
        <section className="account-grid">
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
      className={`panel provider-column ${filter && filter !== p ? "dimmed" : ""}`}
      style={providerStyle(p)}
    >
      <div className="provider-title">
        <Brand provider={p} />
        <h2>
          {toggle ? (
            <button
              className="pool-toggle"
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
        <span>
          {accounts.length} {accounts.length === 1 ? "account" : "accounts"}
        </span>
      </div>
      <div className="pool-body">
        <div>
          <div className="allowance">
            {known.length === accounts.length ? (
              <>
                <span>
                  {Math.round(remaining)}
                  <small>%</small>
                </span>
                {accounts.length > 1 && (
                  <span className="allowance-of">
                    of {accounts.length * 100}%
                  </span>
                )}
              </>
            ) : (
              <span className="unknown-allowance">Not reported</span>
            )}
          </div>
          <p className="window-label">
            {accounts.every((a) => a.window === "Weekly window")
              ? "Weekly remaining"
              : "Allowance remaining"}
          </p>
        </div>
        {stats.length > 0 && (
          <dl className="pool-stats">
            {stats.map((s) => (
              <div key={s.label}>
                <dt>{s.label}</dt>
                <dd>{s.value}</dd>
              </div>
            ))}
          </dl>
        )}
      </div>
      <div className="segmented-progress">
        {accounts.map((a) => (
          <div key={a.id}>
            {a.remaining !== null && (
              <span style={{ width: `${a.remaining}%` }} />
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
    <article className="panel account-card" style={providerStyle(a.provider)}>
      <div className="account-heading">
        <div>
          <h3>
            <Private peek>{accountLabel(a)}</Private>
          </h3>
          <span className="plan">{a.plan || providerLabel(a.provider)}</span>
          <span className={`scope-badge ${a.scope}`}>
            {a.scope === "global" ? "Shared" : "Personal"}
          </span>
          {accountState(a) && (
            <span className={`account-state ${a.status}`}>
              {accountState(a)}
            </span>
          )}
        </div>
        <Brand provider={a.provider} small />
      </div>
      {a.windows !== undefined && a.windows.length > 0 ? (
        <div className="quota-windows">
          <QuotaWindows windows={a.windows} />
        </div>
      ) : (
        <div className="account-allowance">
          <span>{a.window}</span>
          <strong>Unknown</strong>
        </div>
      )}
      {a.quotaError !== undefined && a.quotaError !== "" && (
        <p className="quota-error">
          <Private>{a.quotaError}</Private>
        </p>
      )}
    </article>
  );
}
