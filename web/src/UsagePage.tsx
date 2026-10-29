import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { RefreshCw } from "lucide-react";
import { errorMessage, isStale, type APIRequest } from "./api";
import { CostChart, series, seriesKey } from "./CostChart";
import { OwnerTabs, ownerQuery, type Owner } from "./OwnerTabs";
import { caption, nextPanel, pageNote, pageToolbar } from "./pageStyles";
import { ProviderBrand, providerLabel } from "./ProviderBrand";
import { Breakdown, CostSplits, type View } from "./UsageGroupTable";
import {
  compact,
  money,
  share,
  summarize,
  timeZone,
  type Tally,
  type Usage,
  type UsageReport,
} from "./usage";
import {
  cx,
  empty,
  loading as loadingClass,
  notice,
  noticeAction,
  panelFrame,
} from "./ui";

type Props = {
  // Admins can see everyone's usage.
  admin: boolean;
  request: APIRequest;
  reloadKey: string;
};

const number = (n: number) => n.toLocaleString();
const plural = (n: number, noun: string) =>
  `${number(n)} ${noun}${n === 1 ? "" : "s"}`;
// Loads the usage report again whenever reloadKey or the owner changes.
function useUsage(request: APIRequest, reloadKey: string, owner: Owner) {
  const [data, setData] = useState<UsageReport | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const sequence = useRef(0);

  const load = useCallback(async () => {
    const current = ++sequence.current;
    setLoading(true);
    try {
      const zone = encodeURIComponent(timeZone());
      const query = ownerQuery(owner);
      const result = await request<UsageReport>(
        `/api/usage?tz=${zone}${query ? `&${query}` : ""}`,
      );
      if (current !== sequence.current) return;
      setData(result);
      setError("");
    } catch (err) {
      if (current !== sequence.current || isStale(err)) return;
      setError(errorMessage(err, "Could not load usage."));
    } finally {
      if (current === sequence.current) setLoading(false);
    }
  }, [request, owner]);
  useEffect(() => {
    void load();
  }, [reloadKey, load]);

  return { data, loading, error, load };
}

export function UsagePage({ admin, request, reloadKey }: Props) {
  const [owner, setOwner] = useState<Owner>("mine");
  const { data, loading, error, load } = useUsage(request, reloadKey, owner);
  const [view, setView] = useState<View>("model");

  const usage = useMemo(
    () => (data && data.total.requests > 0 ? summarize(data) : null),
    [data],
  );
  const total = usage?.total;

  return (
    <div>
      {admin && (
        <div className={pageToolbar}>
          <OwnerTabs owner={owner} setOwner={setOwner} label="Show usage of" />
          {owner === "all" && (
            <p className={pageNote}>
              Requests other people sent through their personal accounts show no
              account name.
            </p>
          )}
        </div>
      )}
      {error && (
        <div className={cx(notice({ error: true }), "my-3.75")} role="alert">
          {error}{" "}
          <button className={noticeAction} onClick={() => void load()}>
            Try again
          </button>
        </div>
      )}
      {loading && !data && !error && (
        <div className={loadingClass} role="status" aria-label="Loading">
          <RefreshCw size={22} className="animate-spin" />
        </div>
      )}
      {data && !usage && <p className={cx(empty, "mt-6 mb-1")}>No usage yet</p>}
      {data && usage && total && (
        <>
          <UsageSummary usage={usage} total={total} />

          <UsageTotals total={total} />

          {total.cost > 0 && <CostSplits usage={usage} total={total} />}

          <Breakdown
            view={view}
            setView={setView}
            usage={usage}
            requests={data.requests}
            userNames={data.userNames}
          />
        </>
      )}
    </div>
  );
}

function UsageSummary({ usage, total }: { usage: Usage; total: Tally }) {
  return (
    // Keeps its padding on phones.
    <section
      className={cx(
        panelFrame,
        "grid grid-cols-[minmax(0,320px)_minmax(0,1fr)] gap-x-14 gap-y-7 px-6 pt-6 pb-5.5 max-lg:grid-cols-[1fr]",
      )}
    >
      <div>
        <p className="text-[52px] leading-[1.05] font-semibold tracking-[-1.8px]">
          {money(total.cost)}
        </p>
        <p className={cx(caption, "mt-2.5")}>
          {plural(total.requests, "request")} at API list price
          {total.unpriced > 0 && (
            <span className="block">{number(total.unpriced)} unpriced</span>
          )}
        </p>
        <ul className="mt-7 grid gap-4.5 border-t border-border pt-5.5">
          {usage.providers.map((p) => (
            <li
              key={p.name}
              className="grid grid-cols-[auto_auto_auto_1fr_auto] items-center gap-x-2.25 text-[14px]"
              style={series(p.provider)}
            >
              <span className={seriesKey} aria-hidden="true" />
              <ProviderBrand provider={p.provider} small />
              <strong className="font-[550]">
                {providerLabel(p.provider)}
              </strong>
              <span className="text-[12px] text-muted">
                {plural(p.requests, "request")}
              </span>
              <b className="font-[550]">{money(p.cost)}</b>
              <p className="col-span-full mt-1.25 text-[12px] text-muted">
                {[
                  share(p.cost, total.cost) &&
                    `${share(p.cost, total.cost)} of cost`,
                  `${compact(p.tokens)} tokens`,
                ]
                  .filter(Boolean)
                  .join(" · ")}
              </p>
            </li>
          ))}
        </ul>
      </div>
      <CostChart usage={usage} />
    </section>
  );
}

function UsageTotals({ total }: { total: Tally }) {
  const totals = [
    ["Tokens", compact(total.tokens)],
    ["Cached input", compact(total.cached)],
    ["Uncached input", compact(total.uncached)],
    ["Output", compact(total.output)],
    ["Cache savings", money(total.saved)],
  ];
  return (
    // Five columns divided by rules, keeping their padding on phones, where
    // they pair up and only the second of each pair is ruled.
    <dl
      className={cx(
        nextPanel,
        "grid grid-cols-5 gap-5 px-6 py-5.5 max-sm:grid-cols-[1fr_1fr]",
      )}
    >
      {totals.map(([label, value], i) => (
        <div
          key={label}
          className={cx(
            i > 0 && "border-l border-border pl-5",
            i > 0 &&
              (i % 2 === 1 ? "max-sm:pl-4" : "max-sm:border-l-0 max-sm:pl-0"),
          )}
        >
          <dt className="text-[13px] text-muted">{label}</dt>
          <dd className="mt-2 text-[24px] font-semibold tracking-[-0.6px]">
            {value}
          </dd>
        </div>
      ))}
    </dl>
  );
}
