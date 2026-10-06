import { useCallback, useEffect, useRef, useState } from "react";
import { RefreshCw } from "lucide-react";
import { errorMessage, isStale, statusOf, type APIRequest } from "./api";
import { Private } from "./Privacy";
import { Select } from "./Select";
import {
  filterRequests,
  formatDuration,
  keyLabel,
  resultLabel,
  unknownUsage,
  type OutcomeFilter,
  type RequestRecord,
  type Telemetry,
} from "./telemetry";

type Props = {
  request: APIRequest;
  demo: boolean;
  reloadKey: string;
};

const number = (n: number) => n.toLocaleString();
const outcomes: { value: OutcomeFilter; label: string }[] = [
  { value: "all", label: "All" },
  { value: "error", label: "Errors" },
  { value: "incomplete", label: "Cut short" },
];

function when(value: string) {
  const date = new Date(value);
  const time = date.toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
  if (date.toDateString() === new Date().toDateString()) return time;
  return `${date.toLocaleDateString(undefined, { month: "short", day: "numeric" })}, ${time}`;
}

export function RequestsPage({ request, demo, reloadKey }: Props) {
  const [data, setData] = useState<Telemetry | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [unavailable, setUnavailable] = useState(false);
  const [outcome, setOutcome] = useState<OutcomeFilter>("all");
  const [key, setKey] = useState("");
  const sequence = useRef(0);

  const load = useCallback(async () => {
    const current = ++sequence.current;
    setLoading(true);
    try {
      const result = await request<Telemetry>("/api/telemetry");
      if (current !== sequence.current) return;
      setData({ ...result, requests: result.requests || [] });
      setError("");
      setUnavailable(false);
    } catch (err) {
      if (current !== sequence.current || isStale(err)) return;
      // Demo mode has no request log to read.
      if (demo && statusOf(err) === 409) setUnavailable(true);
      else setError(errorMessage(err, "Could not load the request log."));
    } finally {
      if (current === sequence.current) setLoading(false);
    }
  }, [request, demo]);
  useEffect(() => {
    void load();
  }, [reloadKey, load]);

  const all = data?.requests || [];
  const keyNames = [...new Set(all.map(keyLabel))].sort();
  const selectedKey = keyNames.includes(key) ? key : "";
  const rows = filterRequests(all, outcome, selectedKey);
  const unknown = unknownUsage(all);

  return (
    <div className="requests">
      {unavailable && (
        <div className="notice">
          Demo data. Requests are not forwarded, so there is no log.
        </div>
      )}
      {error && (
        <div className="notice error" role="alert">
          {error} <button onClick={() => void load()}>Try again</button>
        </div>
      )}
      {loading && !data && !error && (
        <div className="empty">
          <RefreshCw size={22} className="spinning" />
        </div>
      )}
      {data && all.length === 0 && (
        <div className="empty">
          <h2>No requests yet</h2>
          <p>
            Calls made with this gateway's API keys show up here. vrouter
            records the model, timing and token counts, never prompts or
            responses.
          </p>
        </div>
      )}
      {data && all.length > 0 && (
        <>
          <dl className="request-totals">
            <div>
              <dt>Requests</dt>
              <dd>{number(data.totals.requests)}</dd>
            </div>
            <div>
              <dt>Input tokens</dt>
              <dd>{number(data.totals.inputTokens)}</dd>
            </div>
            <div>
              <dt>Output tokens</dt>
              <dd>{number(data.totals.outputTokens)}</dd>
            </div>
            <div>
              <dt>Total tokens</dt>
              <dd>{number(data.totals.totalTokens)}</dd>
            </div>
          </dl>
          <p className="requests-note">
            Totals cover the {number(all.length)} most recent{" "}
            {all.length === 1 ? "request" : "requests"} kept in the log, which
            holds at most {number(data.retentionLimit)}. Lifetime counts are on
            each key under <a href="#keys">API keys</a>.
            {unknown > 0 &&
              ` ${number(unknown)} ${unknown === 1 ? "request" : "requests"} reported no usage and ${unknown === 1 ? "adds" : "add"} nothing to the token totals.`}
          </p>
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
            <p className="account-pool-empty">No requests match this filter.</p>
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
      )}
    </div>
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
        <span className={`request-result ${r.outcome}`}>
          <span className="check-state" aria-hidden="true" />
          {resultLabel(r)}
        </span>
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
          title="The provider sent no usage for this request. This is not a count of zero."
        >
          Usage not reported
        </td>
      )}
    </tr>
  );
}
