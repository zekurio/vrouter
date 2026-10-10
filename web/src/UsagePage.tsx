import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { RefreshCw } from "lucide-react";
import { errorMessage, isStale, type APIRequest } from "./api";
import { CostChart, series } from "./CostChart";
import { ProviderBrand, providerLabel } from "./ProviderBrand";
import { RequestLog } from "./RequestLog";
import type { RequestRecord, Telemetry } from "./telemetry";
import {
  compact,
  costTypeLabel,
  costTypes,
  dayLabel,
  money,
  share,
  summarize,
  type Group,
  type Tally,
  type Usage,
} from "./usage";

type Props = {
  request: APIRequest;
  reloadKey: string;
};
type View = "model" | "key" | "day" | "requests";
// An empty log can arrive as null.
type TelemetryReply = Omit<Telemetry, "requests"> & {
  requests: RequestRecord[] | null;
};

const number = (n: number) => n.toLocaleString();
const plural = (n: number, noun: string) =>
  `${number(n)} ${noun}${n === 1 ? "" : "s"}`;
const views: { value: View; label: string }[] = [
  { value: "model", label: "Model" },
  { value: "key", label: "Key" },
  { value: "day", label: "Day" },
  { value: "requests", label: "Requests" },
];

// Loads the request log again whenever reloadKey changes.
function useTelemetry(request: APIRequest, reloadKey: string) {
  const [data, setData] = useState<Telemetry | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const sequence = useRef(0);

  const load = useCallback(async () => {
    const current = ++sequence.current;
    setLoading(true);
    try {
      const result = await request<TelemetryReply>("/api/telemetry");
      if (current !== sequence.current) return;
      setData({ ...result, requests: result.requests ?? [] });
      setError("");
    } catch (err) {
      if (current !== sequence.current || isStale(err)) return;
      setError(errorMessage(err, "Could not load usage."));
    } finally {
      if (current === sequence.current) setLoading(false);
    }
  }, [request]);
  useEffect(() => {
    void load();
  }, [reloadKey, load]);

  return { data, loading, error, load };
}

export function UsagePage({ request, reloadKey }: Props) {
  const { data, loading, error, load } = useTelemetry(request, reloadKey);
  const [view, setView] = useState<View>("model");

  const all = data?.requests;
  const usage = useMemo(
    () => (all !== undefined && all.length > 0 ? summarize(all) : null),
    [all],
  );
  const total = usage?.total;

  return (
    <div className="usage">
      {error && (
        <div className="notice error" role="alert">
          {error} <button onClick={() => void load()}>Try again</button>
        </div>
      )}
      {loading && !data && !error && (
        <div className="loading" role="status" aria-label="Loading">
          <RefreshCw size={22} className="spinning" />
        </div>
      )}
      {data && !usage && <p className="empty">No usage yet</p>}
      {data && usage && total && (
        <>
          <UsageSummary
            usage={usage}
            total={total}
            retentionLimit={data.retentionLimit}
          />

          <UsageTotals total={total} />

          {total.cost > 0 && <CostByType total={total} />}

          <Breakdown
            view={view}
            setView={setView}
            usage={usage}
            requests={data.requests}
          />
        </>
      )}
    </div>
  );
}

function Breakdown({
  view,
  setView,
  usage,
  requests,
}: {
  view: View;
  setView: (view: View) => void;
  usage: Usage;
  requests: RequestRecord[];
}) {
  return (
    <section className="usage-breakdown">
      <div className="section-toolbar">
        <h2>Breakdown</h2>
        <div className="tabs" role="group" aria-label="Break down by">
          {views.map((v) => (
            <button
              key={v.value}
              aria-pressed={view === v.value}
              className={view === v.value ? "selected" : ""}
              onClick={() => setView(v.value)}
            >
              {v.label}
            </button>
          ))}
        </div>
      </div>
      {view === "requests" ? (
        <RequestLog requests={requests} />
      ) : (
        <GroupTable
          view={view}
          groups={
            view === "model"
              ? usage.models
              : view === "key"
                ? usage.keys
                : usage.days
          }
          total={usage.total.cost}
        />
      )}
    </section>
  );
}

function UsageSummary({
  usage,
  total,
  retentionLimit,
}: {
  usage: Usage;
  total: Tally;
  // The most requests the log keeps.
  retentionLimit: number;
}) {
  return (
    <section className="usage-summary">
      <div>
        <p className="usage-total">{money(total.cost)}</p>
        <p className="usage-caption">
          {total.requests >= retentionLimit
            ? `Last ${plural(total.requests, "request")}`
            : plural(total.requests, "request")}{" "}
          at API list price
          {total.unpriced > 0 && <span>{number(total.unpriced)} unpriced</span>}
        </p>
        <ul className="usage-providers">
          {usage.providers.map((p) => (
            <li key={p.name} style={series(p.provider)}>
              <span className="series-key" aria-hidden="true" />
              <ProviderBrand provider={p.provider} small />
              <strong>{providerLabel(p.provider)}</strong>
              <span>{plural(p.requests, "request")}</span>
              <b>{money(p.cost)}</b>
              <p>
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
  return (
    <dl className="usage-totals">
      <div>
        <dt>Tokens</dt>
        <dd>{compact(total.tokens)}</dd>
      </div>
      <div>
        <dt>Cached input</dt>
        <dd>{compact(total.cached)}</dd>
      </div>
      <div>
        <dt>Uncached input</dt>
        <dd>{compact(total.uncached)}</dd>
      </div>
      <div>
        <dt>Output</dt>
        <dd>{compact(total.output)}</dd>
      </div>
      <div>
        <dt>Cache savings</dt>
        <dd>{money(total.saved)}</dd>
      </div>
    </dl>
  );
}

function CostByType({ total }: { total: Tally }) {
  return (
    <section className="usage-types" aria-label="Cost by type">
      <h2>Cost by type</h2>
      <div className="type-bar" aria-hidden="true">
        {costTypes.map(
          (type) =>
            total.byType[type] > 0 && (
              <span
                key={type}
                className={type}
                style={{ flexGrow: total.byType[type] }}
                title={`${costTypeLabel[type]} ${money(total.byType[type])}`}
              />
            ),
        )}
      </div>
      <ul>
        {costTypes.map(
          (type) =>
            total.byType[type] > 0 && (
              <li key={type}>
                <span className={`type-key ${type}`} />
                {costTypeLabel[type]}
                <strong>{money(total.byType[type])}</strong>
              </li>
            ),
        )}
      </ul>
    </section>
  );
}

function GroupTable({
  view,
  groups,
  total,
}: {
  view: Exclude<View, "requests">;
  groups: Group[];
  total: number;
}) {
  const peak = Math.max(...groups.map((g) => g.cost), 0);
  return (
    <div className="table-scroll">
      <table className="usage-table">
        <thead>
          <tr>
            <th scope="col">
              {view === "model" ? "Model" : view === "key" ? "Key" : "Day"}
            </th>
            <th scope="col" className="numeric">
              Cost
            </th>
            <th scope="col" className="numeric">
              Share
            </th>
            <th scope="col" className="numeric">
              Requests
            </th>
            <th scope="col" className="numeric">
              Tokens
            </th>
          </tr>
        </thead>
        <tbody>
          {groups.map((g) => (
            <tr key={`${g.provider}/${g.name}`} style={series(g.provider)}>
              <th scope="row">
                <span className="usage-name">
                  {view === "model" && (
                    <ProviderBrand provider={g.provider} small />
                  )}
                  {view === "day" ? dayLabel(Number(g.name)) : g.name}
                </span>
                {peak > 0 && g.cost > 0 && (
                  <span
                    className="usage-bar"
                    style={{ width: `${(g.cost / peak) * 100}%` }}
                  />
                )}
              </th>
              {g.measured > 0 && g.unpriced === g.measured ? (
                <td className="numeric unpriced" colSpan={2}>
                  Unpriced
                </td>
              ) : (
                <>
                  <td className="numeric cost">
                    {g.measured > 0 && money(g.cost)}
                  </td>
                  <td className="numeric">{share(g.cost, total)}</td>
                </>
              )}
              <td className="numeric">{number(g.requests)}</td>
              <td className="numeric">{g.measured > 0 && compact(g.tokens)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
