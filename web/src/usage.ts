// What the logged requests would cost on each provider's public API.

import { keyLabel, type RequestRecord, type Speed } from "./telemetry";

// Dollars per million tokens. Cache writes default to the input rate.
type Rate = { input: number; read: number; write?: number; output: number };
type ModelRate = Rate & { longContext?: Rate & { above: number } };

// Published list prices, checked October 2026. Subscription traffic is not
// billed this way; these only size the estimate.
// Claude: https://platform.claude.com/docs/en/about-claude/pricing
const rates: Record<string, ModelRate> = {
  "claude-fable-5-1": { input: 10, read: 0.25, write: 12.5, output: 50 },
  "claude-fable-5": { input: 10, read: 1, write: 12.5, output: 50 },
  "claude-opus-5-5": { input: 4, read: 0.2, write: 5, output: 20 },
  "claude-opus-5": { input: 5, read: 0.5, write: 6.25, output: 25 },
  "claude-opus-4-1": { input: 15, read: 1.5, write: 18.75, output: 75 },
  "claude-opus-4": { input: 5, read: 0.5, write: 6.25, output: 25 },
  "claude-sonnet-5-5": { input: 2, read: 0.2, write: 2.5, output: 10 },
  "claude-sonnet-5": { input: 2, read: 0.2, write: 2.5, output: 10 },
  "claude-sonnet-4": { input: 3, read: 0.3, write: 3.75, output: 15 },
  "claude-haiku-5-5": {
    input: 0.1,
    read: 0.01,
    write: 0.125,
    output: 0.5,
    longContext: {
      above: 100_000,
      input: 0.5,
      read: 0.05,
      write: 0.625,
      output: 2.5,
    },
  },
  "claude-haiku-4-5": { input: 1, read: 0.1, write: 1.25, output: 5 },
  "gpt-6-astra": { input: 10, read: 1, output: 50 },
  "gpt-6.1-sol": { input: 2, read: 0.1, output: 10 },
  "gpt-6-sol": { input: 2, read: 0.2, output: 10 },
  "gpt-6-luna": { input: 0.1, read: 0.01, output: 0.5 },
};
// Longest ID first, so a dated or suffixed ID matches its own family.
const known = Object.entries(rates).toSorted(([a], [b]) => b.length - a.length);

// Premium tiers multiply every rate above, cache reads and writes included.
// Claude fast mode (Opus 5.5, 5 and 4.8) and OpenAI Fast, formerly Priority,
// on GPT-6 cost twice the standard rate. OpenAI Ultrafast on gpt-6-astra and
// gpt-6.1-sol costs six times.
// OpenAI: https://developers.openai.com/api/docs/pricing
const speedMultiplier: Record<Speed, number> = { fast: 2, ultrafast: 6 };

function rateFor(record: RequestRecord) {
  const id = (record.nativeModel || record.model).toLowerCase();
  const match = known.find(([k]) => id === k || id.startsWith(k));
  if (match === undefined) return null;
  const [, rate] = match;
  // Prompt length includes cache reads and writes, already in inputTokens.
  return rate.longContext && record.inputTokens > rate.longContext.above
    ? rate.longContext
    : rate;
}

export const costTypes = ["input", "read", "write", "output"] as const;
export type CostType = (typeof costTypes)[number];
export const costTypeLabel: Record<CostType, string> = {
  input: "Input",
  read: "Cache read",
  write: "Cache write",
  output: "Output",
};
export type Tier = "standard" | Speed;

export type Tally = {
  requests: number;
  // Requests with a complete usage report. Only these add tokens and cost.
  measured: number;
  // Measured requests whose model has no known price.
  unpriced: number;
  cost: number;
  byType: Record<CostType, number>;
  // Cost by speed tier, then by provider.
  byTier: Record<Tier, Record<string, number>>;
  // What the cached input would have cost at the full input rate.
  saved: number;
  tokens: number;
  uncached: number;
  cached: number;
  output: number;
};

const tally = (): Tally => ({
  requests: 0,
  measured: 0,
  unpriced: 0,
  cost: 0,
  byType: { input: 0, read: 0, write: 0, output: 0 },
  byTier: { standard: {}, fast: {}, ultrafast: {} },
  saved: 0,
  tokens: 0,
  uncached: 0,
  cached: 0,
  output: 0,
});

function add(into: Tally, r: RequestRecord) {
  into.requests++;
  if (!r.usageKnown) return;
  into.measured++;
  // Cache reads and writes are subsets of the input count.
  const read = Math.min(r.cachedTokens, r.inputTokens);
  const write = Math.min(r.cacheWriteTokens ?? 0, r.inputTokens - read);
  const input = r.inputTokens - read - write;
  into.tokens += r.inputTokens + r.outputTokens;
  into.uncached += input + write;
  into.cached += read;
  into.output += r.outputTokens;
  const rate = rateFor(r);
  if (!rate) {
    into.unpriced++;
    return;
  }
  // Rates are per million tokens.
  const scale = (r.speed ? speedMultiplier[r.speed] : 1) / 1e6;
  const part = {
    input: input * rate.input * scale,
    read: read * rate.read * scale,
    write: write * (rate.write ?? rate.input) * scale,
    output: r.outputTokens * rate.output * scale,
  };
  let cost = 0;
  for (const type of costTypes) {
    into.byType[type] += part[type];
    cost += part[type];
  }
  into.cost += cost;
  const tier = into.byTier[r.speed ?? "standard"];
  tier[r.provider] = (tier[r.provider] ?? 0) + cost;
  into.saved += read * (rate.input - rate.read) * scale;
}

export type Group = Tally & { name: string; provider: string };
export type Bucket = { start: number; byProvider: Record<string, number> };
export const intervals = ["hour", "day", "week", "month"] as const;
export type Interval = (typeof intervals)[number];
export type Usage = {
  total: Tally;
  providers: Group[];
  models: Group[];
  keys: Group[];
  days: Group[];
  series: Record<Interval, Bucket[]>;
  // A log younger than two days reads better by the hour.
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

const startTime = (r: RequestRecord) => new Date(r.startedAt).getTime();

export function summarize(records: RequestRecord[], now = Date.now()): Usage {
  const total = tally();
  const groups = {
    providers: new Map<string, Group>(),
    models: new Map<string, Group>(),
    keys: new Map<string, Group>(),
    days: new Map<string, Group>(),
  };
  const into = (
    map: Map<string, Group>,
    id: string,
    name: string,
    provider: string,
  ) => {
    let group = map.get(id);
    if (!group) map.set(id, (group = { ...tally(), name, provider }));
    return group;
  };

  const first = Math.min(now, ...records.map((r) => startTime(r)));
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

  records.forEach((r) => {
    const started = startTime(r);
    const model = r.model || "Unknown model";
    const day = floor(started, "day");
    const one = tally();
    add(one, r);
    add(total, r);
    // A request refused before routing belongs to no provider.
    if (r.provider)
      add(into(groups.providers, r.provider, r.provider, r.provider), r);
    // It still counts toward its model, so models group by name alone.
    const byModel = into(groups.models, model, model, r.provider);
    byModel.provider ||= r.provider;
    add(byModel, r);
    add(into(groups.keys, r.keyId || keyLabel(r), keyLabel(r), ""), r);
    add(into(groups.days, String(day), String(day), ""), r);
    for (const interval of intervals) {
      const bucket = series[interval].get(floor(started, interval));
      if (bucket && r.provider)
        bucket.byProvider[r.provider] =
          (bucket.byProvider[r.provider] ?? 0) + one.cost;
    }
  });

  const ranked = (map: Map<string, Group>) =>
    [...map.values()].toSorted(
      (a, b) =>
        b.cost - a.cost || b.tokens - a.tokens || b.requests - a.requests,
    );
  return {
    total,
    providers: ranked(groups.providers),
    models: ranked(groups.models),
    keys: ranked(groups.keys),
    days: [...groups.days.values()].toSorted(
      (a, b) => Number(b.name) - Number(a.name),
    ),
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
