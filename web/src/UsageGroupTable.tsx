// The usage page's breakdowns: cost split by type and by tier, and the panel
// that groups usage by model, key, user, account or day, or lists requests.
import type { CSSProperties } from "react";
import { series } from "./CostChart";
import {
  caption,
  nextPanel,
  sectionHeading,
  sectionToolbar,
} from "./pageStyles";
import { Private } from "./Privacy";
import { ProviderBrand, providerLabel } from "./ProviderBrand";
import { RequestLog } from "./RequestLog";
import { speedLabel, speeds, type RequestRecord } from "./telemetry";
import { cx, tab, tableScroll, tabs } from "./ui";
import {
  compact,
  costTypeLabel,
  costTypes,
  dayLabel,
  money,
  share,
  type Group,
  type Tally,
  type Usage,
} from "./usage";

// What a breakdown table groups the usage by.
export type GroupView = "model" | "key" | "scope" | "user" | "day";
const groupHeading: Record<GroupView, string> = {
  model: "Model",
  key: "Key",
  user: "User",
  scope: "Account type",
  day: "Day",
};

export type View = GroupView | "requests";

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

// Columns after the first are spaced on their left.
const head = (numeric: boolean) =>
  cx(
    "border-b border-border pb-2.75 pl-5.5 text-[12px] font-[450] whitespace-nowrap text-muted first:pl-0",
    numeric ? "text-right tabular-nums" : "text-left",
  );
const cell = (cost = false) =>
  cx(
    "pt-3.25 pb-2.75 pl-5.5 text-right whitespace-nowrap tabular-nums",
    cost ? "font-[550] text-text" : "text-subtle",
  );

// One breakdown of the usage, largest cost first, with a bar for each row.
function GroupTable({
  view,
  groups,
  total,
}: {
  view: GroupView;
  groups: Group[];
  total: number;
}) {
  const peak = Math.max(...groups.map((g) => g.cost), 0);
  return (
    <div className={tableScroll}>
      <table className="w-full min-w-[560px] border-collapse text-[13px]">
        <thead>
          <tr>
            <th scope="col" className={head(false)}>
              {groupHeading[view]}
            </th>
            <th scope="col" className={head(true)}>
              Cost
            </th>
            <th scope="col" className={head(true)}>
              Share
            </th>
            <th scope="col" className={head(true)}>
              Requests
            </th>
            <th scope="col" className={head(true)}>
              Tokens
            </th>
          </tr>
        </thead>
        <tbody>
          {groups.map((g) => (
            <tr
              key={`${g.provider}/${g.name}`}
              className="border-b border-border last:border-b-0"
              style={series(g.provider)}
            >
              <th
                scope="row"
                className="w-[55%] pt-3.25 pb-2.75 text-left font-medium"
              >
                <span className="flex items-center gap-2 wrap-anywhere">
                  {view === "model" && (
                    <ProviderBrand provider={g.provider} small />
                  )}
                  {view === "day" ? (
                    dayLabel(Number(g.name))
                  ) : view === "user" ? (
                    <Private peek>{g.name}</Private>
                  ) : (
                    g.name
                  )}
                </span>
                {peak > 0 && g.cost > 0 && (
                  <span
                    className="mt-2.25 block h-0.75 min-w-0.75 rounded-[2px] bg-provider"
                    style={{ width: `${(g.cost / peak) * 100}%` }}
                  />
                )}
              </th>
              {g.measured > 0 && g.unpriced === g.measured ? (
                <td className={cell()} colSpan={2}>
                  Unpriced
                </td>
              ) : (
                <>
                  <td className={cell(true)}>
                    {g.measured > 0 && money(g.cost)}
                  </td>
                  <td className={cell()}>{share(g.cost, total)}</td>
                </>
              )}
              <td className={cell()}>{number(g.requests)}</td>
              <td className={cell()}>{g.measured > 0 && compact(g.tokens)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function Breakdown({
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
    // Phones narrow the sides and top, but the table keeps its short bottom.
    <section
      className={cx(nextPanel, "px-5.5 pt-5 pb-2 max-sm:px-4.5 max-sm:pt-4.5")}
    >
      <div className={cx(sectionToolbar, "mb-4")}>
        <h2 className={sectionHeading}>Breakdown</h2>
        <div className={tabs(true)} role="group" aria-label="Break down by">
          {shown.map((v) => (
            <button
              key={v.value}
              aria-pressed={view === v.value}
              className={tab(view === v.value)}
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
            <p className={cx(caption, "mt-3 mb-2")}>
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

// Grey steps for cost types, the provider colour for premium speeds, and
// stripes for the fastest.
const tones = {
  input: "bg-[color-mix(in_srgb,var(--color-text)_30%,var(--color-surface))]",
  read: "bg-[color-mix(in_srgb,var(--color-text)_50%,var(--color-surface))]",
  write: "bg-[color-mix(in_srgb,var(--color-text)_72%,var(--color-surface))]",
  output: "bg-text",
  standard:
    "bg-[color-mix(in_srgb,var(--color-text)_30%,var(--color-surface))]",
  premium: "bg-provider",
  ultrafast:
    "bg-[repeating-linear-gradient(135deg,var(--color-provider)_0_2px,transparent_2px_4px)]",
};

type Part = {
  key: string;
  label: string;
  cost: number;
  // Colours the segment and its legend key.
  tone: keyof typeof tones;
  style?: CSSProperties;
};

const sum = (costs: Record<string, number>) =>
  Object.values(costs).reduce((a, b) => a + b, 0);

export function CostSplits({ usage, total }: { usage: Usage; total: Tally }) {
  // Premium spend splits by provider, in the chart's order and colours.
  const premium: Part[] = speeds.flatMap((speed) =>
    usage.providers.flatMap((p) => {
      const cost = total.byTier[speed][p.name] ?? 0;
      if (cost <= 0) return [];
      return {
        key: `${speed}/${p.name}`,
        label: `${providerLabel(p.provider)} ${speedLabel[speed].toLowerCase()}`,
        cost,
        tone: speed === "ultrafast" ? "ultrafast" : "premium",
        style: series(p.provider),
      };
    }),
  );
  return (
    // Keeps its padding on phones.
    <div
      className={cx(
        nextPanel,
        "grid grid-cols-2 gap-x-14 gap-y-6.5 px-6 pt-5.5 pb-6 max-lg:grid-cols-[1fr]",
      )}
    >
      <CostSplit
        title="Cost by type"
        parts={costTypes
          .filter((type) => total.byType[type] > 0)
          .map((type) => ({
            key: type,
            label: costTypeLabel[type],
            cost: total.byType[type],
            tone: type,
          }))}
      />
      <CostSplit
        title="Cost by tier"
        parts={[
          {
            key: "standard",
            label: "Standard",
            cost: sum(total.byTier.standard),
            tone: "standard",
          },
          // Listed at zero, so no premium spend reads as checked.
          ...(premium.length > 0
            ? premium
            : [
                {
                  key: "fast",
                  label: "Fast",
                  cost: 0,
                  tone: "premium",
                } satisfies Part,
              ]),
        ]}
      />
    </div>
  );
}

function CostSplit({ title, parts }: { title: string; parts: Part[] }) {
  return (
    <section aria-label={title}>
      <h2 className={sectionHeading}>{title}</h2>
      <div className="mt-4 flex h-2.5 gap-0.5" aria-hidden="true">
        {parts.map(
          (p) =>
            p.cost > 0 && (
              <span
                key={p.key}
                className={cx(
                  "min-w-0.5 basis-0 rounded-[2px] first:rounded-l-[5px] last:rounded-r-[5px]",
                  tones[p.tone],
                )}
                style={{ ...p.style, flexGrow: p.cost }}
                title={`${p.label} ${money(p.cost)}`}
              />
            ),
        )}
      </div>
      <ul className="mt-3.5 flex flex-wrap gap-x-5.5 gap-y-2 text-[13px] text-muted">
        {parts.map((p) => (
          <li key={p.key} className="flex items-center gap-1.75">
            <span
              className={cx("size-2 rounded-[2px]", tones[p.tone])}
              style={p.style}
            />
            {p.label}
            <strong className="font-[550] text-text">{money(p.cost)}</strong>
          </li>
        ))}
      </ul>
    </section>
  );
}
