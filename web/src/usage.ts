// The usage report from /api/usage and what the page derives from it. The
// gateway prices each request at public API list prices when it finishes and
// keeps running totals. Subscription traffic is not billed this way; the
// prices only size the estimate.

import type { RequestRecord, Speed } from "./telemetry";

export const costTypes = ["input", "read", "write", "output"] as const;
export type CostType = (typeof costTypes)[number];
export const costTypeLabel: Record<CostType, string> = {
  input: "Input",
  read: "Cache read",
  write: "Cache write",
  output: "Output",
};
export type Tier = "standard" | Speed;

// What every row of the page shows.
export type Counts = {
  requests: number;
  // Requests with a complete usage report. Only these add tokens and cost.
  measured: number;
  // Measured requests whose model has no known price.
  unpriced: number;
  cost: number;
  tokens: number;
};
export type Tally = Counts & {
  byType: Record<CostType, number>;
  // Cost by speed tier, then by provider.
  byTier: Record<Tier, Record<string, number>>;
  // What the cached input would have cost at the full input rate.
  saved: number;
  uncached: number;
  cached: number;
  output: number;
};

export type Group = Counts & { name: string; provider: string };
export type Bucket = { start: number; byProvider: Record<string, number> };
// A local day or hour that saw requests.
export type Period = Counts & Bucket;
export type UsageReport = {
  total: Tally;
  providers: Group[];
  models: Group[];
  keys: Group[];
  // Oldest first. Hours cover the last two days.
  days: Period[];
  hours: Period[];
  // When the first request finished, in milliseconds. Zero before any.
  first: number;
  // The newest requests, newest first. The totals cover every request,
  // including those the log has dropped.
  requests: RequestRecord[];
  retentionLimit: number;
};

export const intervals = ["hour", "day", "week", "month"] as const;
export type Interval = (typeof intervals)[number];
export type Usage = {
  total: Tally;
  providers: Group[];
  models: Group[];
  keys: Group[];
  days: Group[];
  series: Record<Interval, Bucket[]>;
  // Less than two days of usage reads better by the hour.
  recent: boolean;
};

const HOUR = 3600e3;
function floor(time: number, interval: Interval) {
  const date = new Date(time);
  if (interval === "hour") date.setMinutes(0, 0, 0);
  else date.setHours(0, 0, 0, 0);
  // Weeks start on Monday.
  if (interval === "week")
    date.setDate(date.getDate() - ((date.getDay() + 6) % 7));
  if (interval === "month") date.setDate(1);
  return date.getTime();
}
function next(time: number, interval: Interval) {
  const date = new Date(time);
  if (interval === "hour") date.setHours(date.getHours() + 1);
  else if (interval === "month") date.setMonth(date.getMonth() + 1);
  else date.setDate(date.getDate() + (interval === "week" ? 7 : 1));
  return date.getTime();
}

// The reader's time zone, which decides where the gateway starts a day.
export const timeZone = () => Intl.DateTimeFormat().resolvedOptions().timeZone;

// Lays the report's days and hours out as chart series without gaps.
export function summarize(report: UsageReport, now = Date.now()): Usage {
  const first = Math.min(now, report.first || now);
  const span = (interval: Interval) => {
    const buckets = new Map<number, Bucket>();
    // The hourly series covers the last two days at most.
    const from = interval === "hour" ? Math.max(first, now - 48 * HOUR) : first;
    for (let t = floor(from, interval); t <= now; t = next(t, interval))
      buckets.set(t, { start: t, byProvider: {} });
    return buckets;
  };
  const series = {
    hour: span("hour"),
    day: span("day"),
    week: span("week"),
    month: span("month"),
  };
  const fill = (interval: Interval, period: Period) => {
    const bucket = series[interval].get(floor(period.start, interval));
    if (!bucket) return;
    for (const [provider, cost] of Object.entries(period.byProvider))
      bucket.byProvider[provider] = (bucket.byProvider[provider] ?? 0) + cost;
  };
  for (const hour of report.hours) fill("hour", hour);
  for (const day of report.days) {
    fill("day", day);
    fill("week", day);
    fill("month", day);
  }

  return {
    total: report.total,
    providers: report.providers,
    models: report.models,
    keys: report.keys,
    days: report.days
      .map((day) => ({ ...day, name: String(day.start), provider: "" }))
      .toReversed(),
    series: {
      hour: [...series.hour.values()],
      day: [...series.day.values()],
      week: [...series.week.values()],
      month: [...series.month.values()],
    },
    recent: now - first < 48 * HOUR,
  };
}

export function money(value: number) {
  if (value > 0 && value < 0.01) return "<$0.01";
  return value.toLocaleString(undefined, {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  });
}
export const compact = (value: number) =>
  value.toLocaleString(undefined, {
    notation: "compact",
    maximumSignificantDigits: 3,
  });
export function share(part: number, whole: number) {
  if (!(whole > 0) || !(part > 0)) return "";
  const percent = (part / whole) * 100;
  return percent < 0.1 ? "<0.1%" : `${percent.toFixed(1)}%`;
}

export const dayLabel = (time: number) =>
  new Date(time).toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
  });
export const monthLabel = (time: number) =>
  new Date(time).toLocaleDateString(undefined, {
    month: "short",
    year: "numeric",
  });
export const hourLabel = (time: number) =>
  new Date(time).toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
  });
