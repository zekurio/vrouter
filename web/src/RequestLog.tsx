import { useState } from "react";
import { Private } from "./Privacy";
import { providerLabel } from "./ProviderBrand";
import { Select } from "./Select";
import {
  filterRequests,
  formatDuration,
  keyLabel,
  resultLabel,
  speedLabel,
  type OutcomeFilter,
  type RequestRecord,
} from "./telemetry";
import { dayLabel } from "./usage";

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
  return `${dayLabel(date.getTime())}, ${time}`;
}

export function RequestLog({
  requests,
  userNames,
}: {
  requests: RequestRecord[];
  // Names each request's user in a report of everyone.
  userNames?: Record<string, string> | undefined;
}) {
  const [outcome, setOutcome] = useState<OutcomeFilter>("all");
  const [key, setKey] = useState("");
  const keyNames = [...new Set(requests.map((r) => keyLabel(r)))].toSorted();
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
                <th scope="col" className="request-account">
                  Account
                </th>
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
                <Row
                  key={r.id}
                  record={r}
                  user={userNames?.[r.userId ?? ""] ?? ""}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

function Row({ record: r, user }: { record: RequestRecord; user: string }) {
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
        {r.speed && <span className="cell-sub">{speedLabel[r.speed]}</span>}
      </td>
      <td>
        {keyLabel(r)}
        {user && (
          <span className="cell-sub">
            <Private peek>{user}</Private>
          </span>
        )}
      </td>
      <AccountCell record={r} />
      <ResultCell record={r} />
      <DurationCell record={r} />
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
            r.usagePartial === true
              ? "The response ended without complete accounting. Partial counts are excluded from totals."
              : "The provider sent no usable usage report. This is not a count of zero."
          }
        >
          {r.usagePartial === true
            ? `Incomplete usage (${number(r.totalTokens)} tokens observed)`
            : "Usage not reported"}
        </td>
      )}
    </tr>
  );
}

function AccountCell({ record: r }: { record: RequestRecord }) {
  return (
    <td>
      {r.provider ? providerLabel(r.provider) : "Not routed"}
      {r.accountScope && (
        <span className={`scope-badge ${r.accountScope}`}>
          {r.accountScope === "global" ? "Shared" : "Personal"}
        </span>
      )}
      {r.accountName !== undefined && r.accountName !== "" && (
        <span className="cell-sub">
          <Private peek>{r.accountName}</Private>
        </span>
      )}
    </td>
  );
}

function ResultCell({ record: r }: { record: RequestRecord }) {
  return (
    <td>
      <span
        className={`request-result ${r.outcome}`}
        title={r.status ? `HTTP ${r.status}` : undefined}
      >
        {resultLabel(r)}
      </span>
      {r.failure !== undefined && r.failure !== "" && (
        <span className="cell-sub request-failure" title={r.failure}>
          {r.failure}
        </span>
      )}
    </td>
  );
}

// Streaming is the usual case, so only a buffered response is labelled.
function DurationCell({ record: r }: { record: RequestRecord }) {
  return (
    <td className="numeric">
      {formatDuration(r.durationMs)}
      {r.stream ? (
        r.firstOutputMs !== undefined && (
          <span className="cell-sub" title="Time to first output">
            First output {formatDuration(r.firstOutputMs)}
          </span>
        )
      ) : (
        <span className="cell-sub">Not streamed</span>
      )}
    </td>
  );
}
