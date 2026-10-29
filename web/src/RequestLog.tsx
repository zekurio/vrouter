import { useState } from "react";
import { badgeRow, ScopeBadge } from "./Badge";
import { sectionToolbar } from "./pageStyles";
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
import { cx, empty, tab, tableScroll, tabs } from "./ui";
import { dayLabel } from "./usage";

const number = (n: number) => n.toLocaleString();

const head = (numeric = false) =>
  cx(
    "border-b border-border pr-4.5 pb-2.75 text-[12px] font-[450] whitespace-nowrap text-muted last:pr-0",
    numeric ? "text-right tabular-nums" : "text-left",
  );
// Cells align to the top, so rows with a second line stay level. The time in
// the first column stays on one line.
const cell = (numeric = false, muted = false) =>
  cx(
    "border-b border-border pt-2.75 pr-4.5 pb-2.75 align-top leading-[1.5] wrap-anywhere group-last:border-b-0 first:whitespace-nowrap first:text-muted last:pr-0",
    numeric && "text-right whitespace-nowrap tabular-nums",
    muted ? "text-muted" : "text-subtle",
  );
const subLine = "block text-[11.5px] text-muted";
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
      <div className={cx(sectionToolbar, "mb-4")}>
        <div className={tabs(true)} role="group" aria-label="Filter by result">
          {outcomes.map((o) => (
            <button
              key={o.value}
              aria-pressed={outcome === o.value}
              className={tab(outcome === o.value)}
              onClick={() => setOutcome(o.value)}
            >
              {o.label}
            </button>
          ))}
        </div>
        {keyNames.length > 1 && (
          <Select
            inset
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
        <p className={cx(empty, "mt-6 mb-1")}>No matching requests</p>
      ) : (
        <div className={tableScroll}>
          <table className="w-full min-w-[880px] border-collapse text-[12px]">
            <thead>
              <tr>
                <th scope="col" className={head()}>
                  Time
                </th>
                <th scope="col" className={head()}>
                  Model
                </th>
                <th scope="col" className={head()}>
                  Key
                </th>
                {/* A set width, so hiding or showing addresses does not resize
                    the columns. Longer addresses wrap instead. */}
                <th scope="col" className={cx(head(), "w-[190px]")}>
                  Account
                </th>
                <th scope="col" className={head()}>
                  Result
                </th>
                <th scope="col" className={head(true)}>
                  Duration
                </th>
                <th scope="col" className={head(true)}>
                  Input
                </th>
                <th scope="col" className={head(true)}>
                  Output
                </th>
                <th scope="col" className={head(true)}>
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
    <tr className="group">
      <td className={cell()} title={new Date(r.startedAt).toLocaleString()}>
        {when(r.startedAt)}
      </td>
      <td className={cell()}>
        <span className="font-medium text-text">
          {r.model || "Unknown model"}
        </span>
        {r.nativeModel && r.nativeModel !== r.model && (
          <code className={subLine}>{r.nativeModel}</code>
        )}
        {r.speed && <span className={subLine}>{speedLabel[r.speed]}</span>}
      </td>
      <td className={cell()}>
        {keyLabel(r)}
        {user && (
          <span className={subLine}>
            <Private peek>{user}</Private>
          </span>
        )}
      </td>
      <AccountCell record={r} />
      <ResultCell record={r} />
      <DurationCell record={r} />
      {r.usageKnown ? (
        <>
          <td className={cell(true)}>
            {number(r.inputTokens)}
            {r.cachedTokens > 0 && (
              <span className={subLine}>{number(r.cachedTokens)} cached</span>
            )}
          </td>
          <td className={cell(true)}>{number(r.outputTokens)}</td>
          <td className={cell(true)}>{number(r.totalTokens)}</td>
        </>
      ) : (
        <td
          className={cell(true, true)}
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
    <td className={cell()}>
      <span className={badgeRow}>
        {r.provider ? providerLabel(r.provider) : "Not routed"}
        {r.accountScope && <ScopeBadge scope={r.accountScope} />}
      </span>
      {r.accountName !== undefined && r.accountName !== "" && (
        <span className={subLine}>
          <Private peek>{r.accountName}</Private>
        </span>
      )}
    </td>
  );
}

const resultTone: Record<RequestRecord["outcome"], string> = {
  success: "",
  error: "text-danger",
  incomplete: "text-amber",
};

function ResultCell({ record: r }: { record: RequestRecord }) {
  return (
    <td className={cell()}>
      <span
        className={cx("whitespace-nowrap", resultTone[r.outcome])}
        title={r.status ? `HTTP ${r.status}` : undefined}
      >
        {resultLabel(r)}
      </span>
      {r.failure !== undefined && r.failure !== "" && (
        <span
          className="line-clamp-2 max-w-[260px] text-[11.5px] text-muted"
          title={r.failure}
        >
          {r.failure}
        </span>
      )}
    </td>
  );
}

// Streaming is the usual case, so only a buffered response is labelled.
function DurationCell({ record: r }: { record: RequestRecord }) {
  return (
    <td className={cell(true)}>
      {formatDuration(r.durationMs)}
      {r.stream ? (
        r.firstOutputMs !== undefined && (
          <span className={subLine} title="Time to first output">
            First output {formatDuration(r.firstOutputMs)}
          </span>
        )
      ) : (
        <span className={subLine}>Not streamed</span>
      )}
    </td>
  );
}
