import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
} from "react";
import { RefreshCw } from "lucide-react";
import { errorMessage, isStale, type APIRequest } from "./api";
import { CostChart, series } from "./CostChart";
import { OwnerTabs, ownerQuery, type Owner } from "./OwnerTabs";
import { ProviderBrand, providerLabel } from "./ProviderBrand";
import { RequestLog } from "./RequestLog";
import { GroupTable, type GroupView } from "./UsageGroupTable";
import { speedLabel, speeds, type RequestRecord } from "./telemetry";
import {
  compact,
  costTypeLabel,
  costTypes,
  money,
  share,
  summarize,
  timeZone,
  type Group,
  type Tally,
  type Usage,
  type UsageReport,
} from "./usage";

type Props = {
  // Admins can see everyone's usage.
  admin: boolean;
  request: APIRequest;
  reloadKey: string;
};
type View = GroupView | "requests";

const number = (n: number) => n.toLocaleString();
const plural = (n: number, noun: string) =>
  `${number(n)} ${noun}${n === 1 ? "" : "s"}`;
const views: { value: View; label: string }[] = [
  { value: "model", label: "Model" },
  { value: "key", label: "Key" },
  { value: "user", label: "User" },
  { value: "scope", label: "Account" },
  { value: "day", label: "Day" },
  { value: "requests", label: "Requests" },
];

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
    <div className="usage">
      {admin && (
        <div className="page-toolbar">
          <OwnerTabs owner={owner} setOwner={setOwner} label="Show usage of" />
          {owner === "all" && (
            <p>
              Requests other people sent through their personal accounts show no
              account name.
            </p>
          )}
        </div>
      )}
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

function Breakdown({
  view: chosen,
  setView,
  usage,
  requests,
  userNames,
}: {
  view: View;
  setView: (view: View) => void;
  usage: Usage;
  requests: RequestRecord[];
  userNames: Record<string, string> | undefined;
}) {
  // Only a report of everyone breaks down by user.
  const shown = views.filter(
    (v) => v.value !== "user" || usage.users !== undefined,
  );
  const view = shown.some((v) => v.value === chosen) ? chosen : "model";
  const groups: Record<GroupView, Group[]> = {
    model: usage.models,
    key: usage.keys,
    user: usage.users ?? [],
    scope: usage.scopes,
    day: usage.days,
  };
  return (
    <section className="panel usage-breakdown">
      <div className="section-toolbar">
        <h2>Breakdown</h2>
        <div className="tabs" role="group" aria-label="Break down by">
          {shown.map((v) => (
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
        <>
          <RequestLog requests={requests} userNames={userNames} />
          {/* The totals cover every request. The log keeps only the newest. */}
          {usage.total.requests > requests.length && (
            <p className="usage-caption">
              The latest {number(requests.length)} of{" "}
              {plural(usage.total.requests, "request")}
            </p>
          )}
        </>
      ) : (
        <GroupTable
          view={view}
          groups={groups[view]}
          total={usage.total.cost}
        />
      )}
    </section>
  );
}

function UsageSummary({ usage, total }: { usage: Usage; total: Tally }) {
  return (
    <section className="panel usage-summary">
      <div>
        <p className="usage-total">{money(total.cost)}</p>
        <p className="usage-caption">
          {plural(total.requests, "request")} at API list price
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
    <dl className="panel usage-totals">
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

type Part = {
  key: string;
  label: string;
  cost: number;
  // Colours the segment and its legend key.
  className: string;
  style?: CSSProperties;
};

const sum = (costs: Record<string, number>) =>
  Object.values(costs).reduce((a, b) => a + b, 0);

function CostSplits({ usage, total }: { usage: Usage; total: Tally }) {
  // Premium spend splits by provider, in the chart's order and colours.
  const premium: Part[] = speeds.flatMap((speed) =>
    usage.providers.flatMap((p) => {
      const cost = total.byTier[speed][p.name] ?? 0;
      if (cost <= 0) return [];
      return {
        key: `${speed}/${p.name}`,
        label: `${providerLabel(p.provider)} ${speedLabel[speed].toLowerCase()}`,
        cost,
        className: `premium ${speed}`,
        style: series(p.provider),
      };
    }),
  );
  return (
    <div className="panel usage-costs">
      <CostSplit
        title="Cost by type"
        parts={costTypes
          .filter((type) => total.byType[type] > 0)
          .map((type) => ({
            key: type,
            label: costTypeLabel[type],
            cost: total.byType[type],
            className: type,
          }))}
      />
      <CostSplit
        title="Cost by tier"
        parts={[
          {
            key: "standard",
            label: "Standard",
            cost: sum(total.byTier.standard),
            className: "standard",
          },
          // Listed at zero, so no premium spend reads as checked.
          ...(premium.length > 0
            ? premium
            : [{ key: "fast", label: "Fast", cost: 0, className: "premium" }]),
        ]}
      />
    </div>
  );
}

function CostSplit({ title, parts }: { title: string; parts: Part[] }) {
  return (
    <section className="usage-types" aria-label={title}>
      <h2>{title}</h2>
      <div className="type-bar" aria-hidden="true">
        {parts.map(
          (p) =>
            p.cost > 0 && (
              <span
                key={p.key}
                className={p.className}
                style={{ ...p.style, flexGrow: p.cost }}
                title={`${p.label} ${money(p.cost)}`}
              />
            ),
        )}
      </div>
      <ul>
        {parts.map((p) => (
          <li key={p.key}>
            <span className={`type-key ${p.className}`} style={p.style} />
            {p.label}
            <strong>{money(p.cost)}</strong>
          </li>
        ))}
      </ul>
    </section>
  );
}
