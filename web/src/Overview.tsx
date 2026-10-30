import { accountLabel, accountState, type Account } from "./AccountsPage";
import type { APIRequest } from "./api";
import { badgeRow, ScopeBadge } from "./Badge";
import { card, PoolCard, ProjectionBasis } from "./PoolCard";
import { Private } from "./Privacy";
import { QuotaWindows } from "./Quota";
import {
  ProviderBrand as Brand,
  providerLabel,
  providerStyle,
} from "./ProviderBrand";
import { usePoolForecasts } from "./usePoolForecasts";
import { cx, empty } from "./ui";

// Pools sit in a grid of cards wide enough for their charts, and accounts in
// one of cards at least 300px wide.
const cards = "grid gap-3 max-sm:grid-cols-1";
// The error under an account's windows, as in AccountsPage.
export const quotaError =
  "mt-3.5 text-[12px] leading-[1.6] text-danger wrap-anywhere";

// The account pools, with a card for each account.
export function Overview({
  accounts,
  pool,
  setPool,
  request,
  reloadKey,
}: {
  accounts: Account[];
  // Provider whose accounts are shown alone, or "" for every pool.
  pool: string;
  setPool: (pool: string) => void;
  request: APIRequest;
  // Changes whenever the accounts reload.
  reloadKey: string;
}) {
  const { basis, choose, forecasts } = usePoolForecasts(
    request,
    reloadKey,
    accounts,
  );
  const providers = [...new Set(accounts.map((a) => a.provider))];
  // The pool columns double as the filter. An empty filter shows every pool.
  const filter = providers.length > 1 && providers.includes(pool) ? pool : "";
  const visibleAccounts = accounts.filter(
    (a) => !filter || a.provider === filter,
  );
  return (
    <>
      {[...forecasts.values()].some(Boolean) && (
        <ProjectionBasis basis={basis} choose={choose} />
      )}
      {providers.length > 0 ? (
        <section
          className={cx(
            cards,
            "grid-cols-[repeat(auto-fit,minmax(min(520px,100%),1fr))]",
          )}
          aria-label="Provider pools"
        >
          {providers.map((p) => (
            <PoolCard
              key={p}
              provider={p}
              accounts={accounts.filter((a) => a.provider === p)}
              forecast={forecasts.get(p) ?? null}
              basis={basis}
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
