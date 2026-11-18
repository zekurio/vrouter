import { useState } from "react";
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
import { button, cx, empty, tab, tableScroll, tabs } from "./ui";
import { dayLabel } from "./usage";

const number = (n: number) => n.toLocaleString();

const head = (numeric = false) =>
  cx(
    "border-b border-border pr-4.5 pb-2.75 text-[12px] font-[450] whitespace-nowrap text-muted last:pr-0",
    numeric ? "text-right tabular-nums" : "text-left",
  );
// Cells align to the top, so rows with a second line stay level. The time in
// the first column stays on one line.
//
// Below 720px nine columns do not fit, so each request becomes four short
// lines: the model and the time, the key, the account, and the result with
// the numbers and their units. Second lines of a cell are left out, except
// the name of the account.
const cell = (numeric = false, muted = false) =>
  cx(
    "border-b border-border pt-2.75 pr-4.5 pb-2.75 align-top leading-[1.5] wrap-anywhere group-last:border-b-0 first:whitespace-nowrap first:text-muted last:pr-0",
    "max-md:block max-md:min-w-0 max-md:border-b-0 max-md:p-0 max-md:text-left max-md:wrap-break-word max-md:after:ml-1 max-md:after:text-muted max-md:after:content-[attr(data-unit)]",
    numeric && "text-right whitespace-nowrap tabular-nums",
    muted ? "text-muted" : "text-subtle",
  );
const row =
  "group max-md:flex max-md:flex-wrap max-md:gap-x-3 max-md:gap-y-0.5 max-md:border-b max-md:border-border max-md:py-2.5 max-md:last:border-b-0";
// Where each cell goes among those lines. A failure's text gets lines of its
// own above the numbers.
const place = {
  model: "max-md:order-1 max-md:flex-1",
  time: "max-md:order-2",
  key: "max-md:order-3 max-md:basis-full",
  account: "max-md:order-4 max-md:basis-full",
  result: "max-md:order-5",
  failed: "max-md:order-5 max-md:basis-full",
  number: "max-md:order-6",
};
const subLine = "block text-[12px] text-muted max-md:hidden";
// A second line that stays on a phone, beside the first.
const nameLine = "block text-[12px] text-muted max-md:ml-1.5 max-md:inline";
// Requests drawn at first, and added by each "Show more".
const pageSize = 100;
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

export function RequestLog({ requests }: { requests: RequestRecord[] }) {
  const [outcome, setOutcome] = useState<OutcomeFilter>("all");
  const [key, setKey] = useState("");
  const [limit, setLimit] = useState(pageSize);
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
          <table className="w-full min-w-[880px] border-collapse text-[12px] max-md:block max-md:min-w-0">
            <thead className="max-md:sr-only">
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
            <tbody className="max-md:block">
              {rows.slice(0, limit).map((r) => (
                <Row key={r.id} record={r} />
              ))}
            </tbody>
          </table>
          {rows.length > limit && (
            <button
              className={cx(button("secondary"), "mt-3.5")}
              onClick={() => setLimit(limit + pageSize)}
            >
              Show {Math.min(pageSize, rows.length - limit)} more
            </button>
          )}
        </div>
      )}
    </>
  );
}

function Row({ record: r }: { record: RequestRecord }) {
  return (
    <tr className={row}>
      <td
        className={cx(cell(), place.time)}
        title={new Date(r.startedAt).toLocaleString()}
      >
        {when(r.startedAt)}
      </td>
      <td className={cx(cell(), place.model)}>
        <span className="font-medium text-text max-md:text-[13px]">
          {r.model || "Unknown model"}
        </span>
        {r.nativeModel && r.nativeModel !== r.model && (
          <code className={subLine}>{r.nativeModel}</code>
        )}
        {r.speed && <span className={subLine}>{speedLabel[r.speed]}</span>}
      </td>
      <td className={cx(cell(), place.key)}>{keyLabel(r)}</td>
      <AccountCell record={r} />
      <ResultCell record={r} />
      <DurationCell record={r} />
      {r.usageKnown ? (
        <>
          <td className={cx(cell(true), place.number)} data-unit="in">
            {number(r.inputTokens)}
            {r.cachedTokens > 0 && (
              <span className={subLine}>{number(r.cachedTokens)} cached</span>
            )}
          </td>
          <td className={cx(cell(true), place.number)} data-unit="out">
            {number(r.outputTokens)}
          </td>
          <td className={cx(cell(true), place.number)} data-unit="total">
            {number(r.totalTokens)}
          </td>
        </>
      ) : (
        <td
          className={cx(cell(true, true), place.number)}
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
    <td className={cx(cell(), place.account)}>
      {r.provider ? providerLabel(r.provider) : "Not routed"}
      {r.accountName !== undefined && r.accountName !== "" && (
        <span className={nameLine}>
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
    <td
      className={cx(
        cell(),
        r.failure !== undefined && r.failure !== ""
          ? place.failed
          : place.result,
      )}
    >
      <span
        className={cx("whitespace-nowrap", resultTone[r.outcome])}
        title={r.status ? `HTTP ${r.status}` : undefined}
      >
        {resultLabel(r)}
      </span>
      {r.failure !== undefined && r.failure !== "" && (
        <span
          className="line-clamp-2 max-w-[260px] text-[12px] text-muted max-md:max-w-none"
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
    <td className={cx(cell(true), place.number)}>
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
