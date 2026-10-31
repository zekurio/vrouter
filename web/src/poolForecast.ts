// A pool's allowance over time: what /api/quota-history recorded, and where
// it heads if spending keeps a recent rate. Shares are percent of one
// account's allowance, so a pool of three runs from 0 to 300.

import type { Account } from "./AccountsPage";
import type { QuotaWindow } from "./Quota";

// One run of identical reports. Times are Unix milliseconds.
export type QuotaRun = {
  since: number;
  until: number;
  remaining: number;
  resetAt?: number;
};
export type QuotaHistory = {
  accountId: string;
  window: string;
  runs: QuotaRun[];
};

// A point on a line. Two points at one time are a jump, such as a reset.
export type Point = { t: number; v: number };

export const HOUR = 3600e3;
export const DAY = 24 * HOUR;

// What the projection takes as the coming rate of spending.
export const bases = ["3h", "day", "window"] as const;
export type Basis = (typeof bases)[number];
export const basisLabel: Record<Basis, string> = {
  "3h": "3h",
  day: "24h",
  window: "Since reset",
};
const basisSpan: Record<Basis, number> = {
  "3h": 3 * HOUR,
  day: DAY,
  window: 0,
};

// The window an account leads with: the weekly one, or the first. The
// gateway picks the same one for the account's remaining share.
export function headline(account: Account): QuotaWindow | undefined {
  const windows = account.windows ?? [];
  return windows.find((w) => w.id === "weekly") ?? windows[0];
}

const resetTime = (w: QuotaWindow) =>
  w.resetAt === undefined || w.resetAt === ""
    ? 0
    : new Date(w.resetAt).getTime();
// Claude does not always report a length; its window ids imply one.
function windowLength(w: QuotaWindow) {
  if (w.seconds !== undefined && w.seconds > 0) return w.seconds * 1000;
  return w.id === "five-hour" ? 5 * HOUR : 7 * DAY;
}

// An account's line from its runs, ending at its current report. Between
// runs the share moves in a straight line, except across a reset: the old
// share holds until the reset and the line starts again from full.
function accountLine(runs: QuotaRun[], current: QuotaWindow, now: number) {
  const live = {
    since: now,
    until: now,
    remaining: current.remaining,
    resetAt: resetTime(current),
  };
  const points: Point[] = [];
  let last: QuotaRun | undefined;
  for (const run of [...runs, live]) {
    // A clock that runs behind the gateway's puts the live report early.
    const since = Math.max(run.since, last?.until ?? run.since);
    const reset = last?.resetAt ?? 0;
    if (last && reset > last.until && reset <= since)
      points.push({ t: reset, v: last.remaining }, { t: reset, v: 100 });
    points.push({ t: since, v: run.remaining });
    if (run.until > since) points.push({ t: run.until, v: run.remaining });
    last = { ...run, until: Math.max(run.until, since) };
  }
  return points;
}

// The index of the first point at t or later.
function firstFrom(line: Point[], t: number) {
  let lo = 0;
  let hi = line.length - 1;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if ((line[mid]?.t ?? 0) < t) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}

// The value of a line at t, from before a jump at t or after it. Null
// outside the line.
function valueAt(line: Point[], t: number, after = false): number | null {
  const first = line[0];
  const end = line.at(-1);
  if (!first || !end || t < first.t || t > end.t) return null;
  let i = firstFrom(line, t);
  const at = line[i];
  const before = line[i - 1];
  if (!at) return null;
  if (at.t !== t) {
    if (!before) return at.v;
    return before.v + ((at.v - before.v) * (t - before.t)) / (at.t - before.t);
  }
  if (after) while (line[i + 1]?.t === t) i++;
  return line[i]?.v ?? null;
}

// The sum of the lines over the time all of them cover, from `from` on.
function sumLines(lines: Point[][], from: number) {
  const times = new Set<number>([from]);
  for (const line of lines)
    for (const p of line) if (p.t > from) times.add(p.t);
  const points: Point[] = [];
  for (const t of [...times].toSorted((a, b) => a - b)) {
    let before = 0;
    let after = 0;
    let covered = true;
    for (const line of lines) {
      const left = valueAt(line, t);
      const right = valueAt(line, t, true);
      if (left === null || right === null) {
        covered = false;
        break;
      }
      before += left;
      after += right;
    }
    if (!covered) continue;
    points.push({ t, v: before });
    if (after !== before) points.push({ t, v: after });
  }
  return points;
}

// The drops in a line since `from`, per hour. Rises are resets, not spending.
// Null when the line covers less than a quarter of the span.
function recentRate(line: Point[], from: number, now: number, span: number) {
  const start = Math.max(from, line[0]?.t ?? now);
  if (now - start < span / 4) return null;
  let used = 0;
  let previous = valueAt(line, start, true);
  for (const p of line) {
    if (p.t <= start) continue;
    if (previous !== null && p.v < previous) used += previous - p.v;
    previous = p.v;
  }
  return (used / (now - start)) * HOUR;
}

// The average rate since the window reset, as the account cards pace it.
// Null in the first 3% of a window, which is too little to go on.
function windowRate(w: QuotaWindow, now: number) {
  const length = windowLength(w);
  const reset = resetTime(w);
  if (!reset) return w.remaining >= 100 ? 0 : null;
  const elapsed = length - (reset - now);
  if (!(elapsed >= length * 0.03 && elapsed <= length)) return null;
  return ((100 - w.remaining) / elapsed) * HOUR;
}

// A rise of the line at t, to v.
export type Jump = { t: number; gain: number; v: number };
export type PoolForecast = {
  // The recorded line, up to now, and the projected line from now.
  recorded: Point[];
  projected: Point[];
  // Rises of the recorded and projected lines, mostly resets.
  jumps: Jump[];
  from: number;
  now: number;
  to: number;
  top: number;
  // The pool's spending in percent per hour, and what it was measured over.
  rate: number;
  basis: Basis;
  // When the projected pool runs dry, or null if it lasts the chart.
  empty: number | null;
};

type Simulated = { left: number; reset: number; length: number };

// The next time anything changes: a reset, an account running dry, or the
// end.
function nextChange(
  accounts: Simulated[],
  each: number,
  t: number,
  to: number,
) {
  let next = to;
  for (const a of accounts) {
    if (a.reset > t) next = Math.min(next, a.reset);
    if (each > 0 && a.left > 0) next = Math.min(next, t + a.left / each);
  }
  return next;
}

// Spends from t to the next change. Requests rotate across the accounts that
// have allowance left, so the rate splits evenly between them, and an account
// that runs dry passes its share to the rest. A window that has not started
// starts with the first request.
function spend(accounts: Simulated[], rate: number, t: number, to: number) {
  const live = accounts.filter((a) => a.left > 0);
  const each = live.length > 0 ? rate / live.length / HOUR : 0;
  for (const a of live) if (each > 0 && a.reset === 0) a.reset = t + a.length;
  const next = nextChange(accounts, each, t, to);
  for (const a of live) {
    a.left -= each * (next - t);
    if (a.left < 1e-6) a.left = 0;
  }
  return { next, spent: live.length > 0 };
}

// Refills the accounts whose window has reset by t.
function refill(accounts: Simulated[], t: number) {
  for (const a of accounts) {
    if (a.reset === 0 || a.reset > t) continue;
    a.left = 100;
    a.reset += a.length;
  }
}

// Steps the accounts forward from now to the end of the chart.
function project(accounts: Simulated[], rate: number, now: number, to: number) {
  const total = () => accounts.reduce((n, a) => n + a.left, 0);
  const projected: Point[] = [{ t: now, v: total() }];
  const jumps: Jump[] = [];
  let empty: number | null = total() === 0 ? now : null;
  let t = now;
  for (let step = 0; t < to && step < 500; step++) {
    const { next, spent } = spend(accounts, rate, t, to);
    t = next;
    const before = total();
    projected.push({ t, v: before });
    if (empty === null && before === 0 && spent) empty = t;
    refill(accounts, t);
    const after = total();
    if (after > before) {
      projected.push({ t, v: after });
      jumps.push({ t, gain: after - before, v: after });
    }
  }
  return { projected, jumps, empty };
}

// The recorded and projected line of a pool's headline windows. Accounts
// without a current report are left out.
export function forecast(
  accounts: Account[],
  history: QuotaHistory[],
  basis: Basis,
  now: number,
): PoolForecast | null {
  const known = accounts.flatMap((a) => {
    const w = headline(a);
    if (!w) return [];
    const runs =
      history.find((h) => h.accountId === a.id && h.window === w.id)?.runs ??
      [];
    return [{ window: w, line: accountLine(runs, w, now) }];
  });
  if (known.length === 0) return null;
  const length = Math.max(...known.map((k) => windowLength(k.window)));

  // At least a seventh of the window back, at most the whole window, as far
  // as every account has history.
  const recordedFrom = Math.max(...known.map((k) => k.line[0]?.t ?? now));
  const back = Math.min(Math.max(now - recordedFrom, length / 7), length);
  const from = now - back;
  const to = now + length;
  const recorded = sumLines(
    known.map((k) => k.line),
    from,
  );

  // The basis falls back to the window average when there is too little
  // history for it.
  let used = basis;
  let rates =
    basis === "window"
      ? null
      : known.map((k) =>
          recentRate(k.line, now - basisSpan[basis], now, basisSpan[basis]),
        );
  if (!rates || rates.some((r) => r === null)) {
    used = "window";
    rates = known.map((k) => windowRate(k.window, now));
  }
  const rate = rates.reduce<number>((n, r) => n + (r ?? 0), 0);

  const simulated = known.map((k) => ({
    left: k.window.remaining,
    reset: resetTime(k.window) > now ? resetTime(k.window) : 0,
    length: windowLength(k.window),
  }));
  const { projected, jumps, empty } = project(simulated, rate, now, to);

  // Rises in the recorded line, where windows reset.
  const recordedJumps: Jump[] = [];
  recorded.forEach((p, i) => {
    const previous = recorded[i - 1];
    if (previous && previous.t === p.t && p.v > previous.v)
      recordedJumps.push({ t: p.t, gain: p.v - previous.v, v: p.v });
  });

  return {
    recorded,
    projected,
    jumps: [...recordedJumps, ...jumps],
    from,
    now,
    to,
    top: known.length * 100,
    rate,
    basis: used,
    empty,
  };
}

// The pool's value at t, recorded up to now and projected after. Null where
// nothing was recorded.
export function poolValue(f: PoolForecast, t: number) {
  return t <= f.now ? valueAt(f.recorded, t) : valueAt(f.projected, t);
}
