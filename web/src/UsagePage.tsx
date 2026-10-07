import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type PointerEvent,
} from "react";
import { RefreshCw } from "lucide-react";
import { errorMessage, isStale, type APIRequest } from "./api";
import { Private } from "./Privacy";
import { ProviderBrand } from "./ProviderBrand";
import { Select } from "./Select";
import {
  filterRequests,
  formatDuration,
  keyLabel,
  resultLabel,
  type OutcomeFilter,
  type RequestRecord,
  type Telemetry,
} from "./telemetry";
import {
  compact,
  costTypeLabel,
  costTypes,
  intervals,
  money,
  share,
  summarize,
  type Bucket,
  type Group,
  type Interval,
  type Usage,
} from "./usage";

type Props = {
  request: APIRequest;
  reloadKey: string;
};
type View = "model" | "key" | "day" | "requests";

const number = (n: number) => n.toLocaleString();
const plural = (n: number, noun: string) =>
  `${number(n)} ${noun}${n === 1 ? "" : "s"}`;
const views: { value: View; label: string }[] = [
  { value: "model", label: "Model" },
  { value: "key", label: "Key" },
  { value: "day", label: "Day" },
  { value: "requests", label: "Requests" },
];
const outcomes: { value: OutcomeFilter; label: string }[] = [
  { value: "all", label: "All" },
  { value: "error", label: "Errors" },
  { value: "incomplete", label: "Cut short" },
];

// Chart series follow the provider, so a provider keeps its color in every
// view. Anything else shares the neutral slot.
const seriesColor = (provider: string) =>
  provider === "Codex" || provider === "OpenAI"
    ? "var(--series-1)"
    : provider === "Claude"
      ? "var(--series-2)"
      : "var(--series-other)";
const series = (provider: string) =>
  ({ "--provider": seriesColor(provider) }) as CSSProperties;

const dayLabel = (time: number) =>
  new Date(time).toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
  });
const monthLabel = (time: number) =>
  new Date(time).toLocaleDateString(undefined, {
    month: "short",
    year: "numeric",
  });
const hourLabel = (time: number) =>
  new Date(time).toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
  });
function when(value: string) {
  const date = new Date(value);
  const time = date.toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
  if (date.toDateString() === new Date().toDateString()) return time;
  return `${dayLabel(date.getTime())}, ${time}`;
}

export function UsagePage({ request, reloadKey }: Props) {
  const [data, setData] = useState<Telemetry | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [view, setView] = useState<View>("model");
  const sequence = useRef(0);

  const load = useCallback(async () => {
    const current = ++sequence.current;
    setLoading(true);
    try {
      const result = await request<Telemetry>("/api/telemetry");
      if (current !== sequence.current) return;
      setData({ ...result, requests: result.requests || [] });
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

  const all = data?.requests;
  const usage = useMemo(() => (all?.length ? summarize(all) : null), [all]);
  const total = usage?.total;

  return (
    <div className="usage">
      {error && (
        <div className="notice error" role="alert">
          {error} <button onClick={() => void load()}>Try again</button>
        </div>
      )}
      {loading && !data && !error && (
        <div className="loading">
          <RefreshCw size={22} className="spinning" />
        </div>
      )}
      {data && !usage && <p className="empty">No usage yet</p>}
      {data && usage && total && (
        <>
          <section className="usage-summary">
            <div>
              <p className="usage-total">{money(total.cost)}</p>
              <p className="usage-caption">
                {total.requests >= data.retentionLimit
                  ? `Last ${plural(total.requests, "request")}`
                  : plural(total.requests, "request")}{" "}
                at API list price
                {total.unpriced > 0 && (
                  <span>{number(total.unpriced)} unpriced</span>
                )}
              </p>
              <ul className="usage-providers">
                {usage.providers.map((p) => (
                  <li key={p.name} style={series(p.provider)}>
                    <span className="series-key" aria-hidden="true" />
                    <ProviderBrand provider={p.provider} small />
                    <strong>{p.name}</strong>
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

          {total.cost > 0 && (
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
          )}

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
              <RequestLog requests={data.requests} />
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
                total={total.cost}
              />
            )}
          </section>
        </>
      )}
    </div>
  );
}

const SIZE = { height: 210, left: 46, right: 10, top: 10, bottom: 24 };

// A round step that splits the range into about three intervals.
function tickStep(max: number) {
  const rough = max / 3;
  const power = 10 ** Math.floor(Math.log10(rough));
  const unit = rough / power;
  return (unit <= 1 ? 1 : unit <= 2 ? 2 : unit <= 5 ? 5 : 10) * power;
}
const axisMoney = (value: number) =>
  value === 0
    ? "0"
    : value < 1
      ? `$${value.toFixed(value < 0.1 ? 3 : 2)}`
      : `$${compact(value)}`;

const DAY = 24 * 3600e3;
const intervalLabel: Record<Interval, string> = {
  hour: "Hourly",
  day: "Daily",
  week: "Weekly",
  month: "Monthly",
};
const axisLabel: Record<Interval, (time: number) => string> = {
  hour: hourLabel,
  day: dayLabel,
  week: dayLabel,
  month: monthLabel,
};
const tipLabel: Record<Interval, (time: number) => string> = {
  hour: (time) => `${dayLabel(time)}, ${hourLabel(time)}`,
  day: dayLabel,
  // Noon of the last day, so a daylight-saving shift cannot move the date.
  week: (time) => `${dayLabel(time)} – ${dayLabel(time + 6.5 * DAY)}`,
  month: monthLabel,
};

// A bar segment. Only the end of a stack is rounded.
function segment(x: number, y: number, w: number, h: number, r: number) {
  r = Math.min(r, w / 2, h);
  return `M${x},${y + h}V${y + r}Q${x},${y} ${x + r},${y}H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + h}Z`;
}

function CostChart({ usage }: { usage: Usage }) {
  const frame = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(0);
  const [hover, setHover] = useState<number | null>(null);
  const [chosen, setChosen] = useState<Interval | null>(null);
  useEffect(() => {
    const node = frame.current;
    if (!node) return;
    const observer = new ResizeObserver(() => setWidth(node.clientWidth));
    observer.observe(node);
    return () => observer.disconnect();
  }, []);

  const interval = chosen ?? (usage.recent ? "hour" : "day");
  const buckets = usage.series[interval];
  const providers = usage.providers.filter((p) => p.cost > 0);
  const sum = (b: Bucket) =>
    providers.reduce((n, p) => n + (b.byProvider[p.name] ?? 0), 0);
  const peak = Math.max(...buckets.map(sum), 0);
  const step = tickStep(peak || 1);
  const top = Math.ceil((peak || 1) / step) * step;
  const ticks = Array.from(
    { length: Math.round(top / step) + 1 },
    (_, i) => i * step,
  );
  const inner = Math.max(width - SIZE.left - SIZE.right, 0);
  const plot = SIZE.height - SIZE.top - SIZE.bottom;
  const band = inner / buckets.length;
  const bar = Math.min(Math.max(band - 2, 1), 40);
  const x = (i: number) => SIZE.left + band * (i + 0.5);
  const y = (value: number) => SIZE.top + plot * (1 - value / top);
  const base = y(0);

  function track(event: PointerEvent<SVGSVGElement>) {
    const left = event.currentTarget.getBoundingClientRect().left;
    const index = Math.floor((event.clientX - left - SIZE.left) / (band || 1));
    setHover(Math.min(Math.max(index, 0), buckets.length - 1));
  }
  const at: Bucket | null = hover === null ? null : (buckets[hover] ?? null);
  const marks = [0, Math.floor((buckets.length - 1) / 2), buckets.length - 1];

  return (
    <figure className="usage-chart">
      <figcaption>
        Cost
        <div className="tabs" role="group" aria-label="Chart interval">
          {intervals.map((value) => (
            <button
              key={value}
              aria-pressed={interval === value}
              className={interval === value ? "selected" : ""}
              onClick={() => {
                setHover(null);
                setChosen(value);
              }}
            >
              {intervalLabel[value]}
            </button>
          ))}
        </div>
      </figcaption>
      <div ref={frame}>
        {width > 0 && (
          <svg
            width={width}
            height={SIZE.height}
            role="img"
            aria-label={`${intervalLabel[interval]} cost by provider. The Day breakdown below lists daily values.`}
            onPointerMove={track}
            onPointerLeave={() => setHover(null)}
          >
            {at && hover !== null && (
              <rect
                className="band"
                x={SIZE.left + band * hover}
                y={SIZE.top}
                width={band}
                height={plot}
              />
            )}
            {ticks.map((tick) => (
              <g key={tick}>
                <line
                  className="grid"
                  x1={SIZE.left}
                  x2={width - SIZE.right}
                  y1={y(tick)}
                  y2={y(tick)}
                />
                <text x={SIZE.left - 10} y={y(tick) + 3.5} textAnchor="end">
                  {axisMoney(tick)}
                </text>
              </g>
            ))}
            {[...new Set(marks)].map((i) => (
              <text
                key={i}
                x={
                  buckets.length === 1
                    ? x(i)
                    : i === 0
                      ? SIZE.left
                      : i === buckets.length - 1
                        ? width - SIZE.right
                        : x(i)
                }
                y={SIZE.height - 5}
                textAnchor={
                  buckets.length > 1 && i === 0
                    ? "start"
                    : buckets.length > 1 && i === buckets.length - 1
                      ? "end"
                      : "middle"
                }
              >
                {axisLabel[interval](buckets[i].start)}
              </text>
            ))}
            {buckets.map((b, i) => {
              const parts = providers.filter((p) => b.byProvider[p.name] > 0);
              let floor = base;
              return parts.map((p, n) => {
                // Any cost shows, and stacked segments keep a gap between them.
                const height = Math.max(base - y(b.byProvider[p.name]), 2);
                const gap = n > 0 && height > 3 ? 2 : 0;
                floor -= height;
                return (
                  <path
                    key={`${b.start}/${p.name}`}
                    className="bar"
                    style={series(p.provider)}
                    d={segment(
                      x(i) - bar / 2,
                      floor,
                      bar,
                      height - gap,
                      n === parts.length - 1 ? 4 : 0,
                    )}
                  />
                );
              });
            })}
          </svg>
        )}
        {at && hover !== null && (
          <div
            className={`chart-tip ${x(hover) > width / 2 ? "left" : ""}`}
            style={{ left: x(hover) }}
            role="status"
          >
            <p>{tipLabel[interval](at.start)}</p>
            {providers.map((p) => (
              <div key={p.name} style={series(p.provider)}>
                <span className="series-key" aria-hidden="true" />
                <strong>{money(at.byProvider[p.name] ?? 0)}</strong>
                {p.name}
              </div>
            ))}
            {providers.length > 1 && (
              <div>
                <strong>{money(sum(at))}</strong>
                Total
              </div>
            )}
          </div>
        )}
      </div>
    </figure>
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

function RequestLog({ requests }: { requests: RequestRecord[] }) {
  const [outcome, setOutcome] = useState<OutcomeFilter>("all");
  const [key, setKey] = useState("");
  const keyNames = [...new Set(requests.map(keyLabel))].sort();
  const selectedKey = keyNames.includes(key) ? key : "";
  const rows = filterRequests(requests, outcome, selectedKey);
  return (
    <>
      <div className="section-toolbar">
        <div className="tabs" role="group" aria-label="Filter by result">
          {outcomes.map((o) => (
            <button
              key={o.value}
              aria-pressed={outcome === o.value}
              className={outcome === o.value ? "selected" : ""}
              onClick={() => setOutcome(o.value)}
            >
              {o.label}
            </button>
          ))}
        </div>
        {keyNames.length > 1 && (
          <Select
            label="Filter by key"
            value={selectedKey}
            options={[
              { value: "", label: "All keys" },
              ...keyNames.map((k) => ({ value: k, label: k })),
            ]}
            onChange={setKey}
          />
        )}
      </div>
      {rows.length === 0 ? (
        <p className="empty">No matching requests</p>
      ) : (
        <div className="table-scroll">
          <table className="request-table">
            <thead>
              <tr>
                <th scope="col">Time</th>
                <th scope="col">Model</th>
                <th scope="col">Key</th>
                <th scope="col">Account</th>
                <th scope="col">Result</th>
                <th scope="col" className="numeric">
                  Duration
                </th>
                <th scope="col" className="numeric">
                  Input
                </th>
                <th scope="col" className="numeric">
                  Output
                </th>
                <th scope="col" className="numeric">
                  Total
                </th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <Row key={r.id} record={r} />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

function Row({ record: r }: { record: RequestRecord }) {
  return (
    <tr>
      <td title={new Date(r.startedAt).toLocaleString()}>
        {when(r.startedAt)}
      </td>
      <td>
        <span className="cell-main">{r.model || "Unknown model"}</span>
        {r.nativeModel && r.nativeModel !== r.model && (
          <code>{r.nativeModel}</code>
        )}
      </td>
      <td>{keyLabel(r)}</td>
      <td>
        {r.provider || "Not routed"}
        {r.accountId && (
          <span className="cell-sub">
            <Private>{r.accountId}</Private>
          </span>
        )}
      </td>
      <td>
        <span className={`request-result ${r.outcome}`}>{resultLabel(r)}</span>
        {r.stream && <span className="cell-sub">Streamed</span>}
      </td>
      <td className="numeric">{formatDuration(r.durationMs)}</td>
      {r.usageKnown ? (
        <>
          <td className="numeric">
            {number(r.inputTokens)}
            {r.cachedTokens > 0 && (
              <span className="cell-sub">{number(r.cachedTokens)} cached</span>
            )}
          </td>
          <td className="numeric">{number(r.outputTokens)}</td>
          <td className="numeric">{number(r.totalTokens)}</td>
        </>
      ) : (
        <td
          className="numeric usage-unknown"
          colSpan={3}
          title={
            r.usagePartial
              ? "The response ended without complete accounting. Partial counts are excluded from totals."
              : "The provider sent no usable usage report. This is not a count of zero."
          }
        >
          {r.usagePartial
            ? `Incomplete usage (${number(r.totalTokens)} tokens observed)`
            : "Usage not reported"}
        </td>
      )}
    </tr>
  );
}
