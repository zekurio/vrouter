import { useEffect, useMemo, useState } from "react";
import type { Account } from "./AccountsPage";
import type { APIRequest } from "./api";
import {
  accountRates,
  bases,
  DAY,
  forecast,
  headline,
  type Basis,
  type QuotaHistory,
} from "./poolForecast";
import { readStored, writeStored } from "./storage";

const basisKey = "vrouter-pool-basis";
const storedBasis = (): Basis => {
  const value = readStored(basisKey);
  return bases.find((b) => b === value) ?? "day";
};

// The most windows one history request may ask for, as the gateway allows.
const maxWindows = 8;

// The history of the accounts' windows, loaded again whenever the state
// reloads. A failed load keeps the last history; the charts still have the
// current reports.
function useQuotaHistory(
  request: APIRequest,
  reloadKey: string,
  accounts: Account[],
) {
  const [history, setHistory] = useState<QuotaHistory[]>([]);
  const shown = accounts.flatMap((a) => a.windows ?? []);
  // Headline windows first, as the pools need them; the gateway caps the
  // count.
  const lead = new Set(accounts.map((a) => headline(a)?.id ?? ""));
  const windows = [...new Set(shown.map((w) => w.id))]
    .toSorted((a, b) => Number(lead.has(b)) - Number(lead.has(a)))
    .slice(0, maxWindows)
    .toSorted();
  const longest = Math.max(0, ...shown.map((w) => w.seconds ?? 0)) * 1000;
  const days = Math.min(30, Math.max(1, Math.ceil(longest / DAY)) + 1);
  const query = windows
    .map((w) => `window=${encodeURIComponent(w)}`)
    .concat(`days=${days}`)
    .join("&");
  const wanted = windows.length > 0;
  useEffect(() => {
    if (!wanted) return undefined;
    let current = true;
    const load = async () => {
      try {
        const result = await request<{ history: QuotaHistory[] }>(
          `/api/quota-history?${query}`,
        );
        if (current) setHistory(result.history);
      } catch {
        // Keep the last history.
      }
    };
    void load();
    return () => {
      current = false;
    };
  }, [request, reloadKey, query, wanted]);
  return history;
}

// Each pool's forecast by provider, null for a pool without reports, each
// account's window rates on the same basis, and the basis they project from,
// which the browser remembers.
export function usePoolForecasts(
  request: APIRequest,
  reloadKey: string,
  accounts: Account[],
) {
  const [basis, setBasis] = useState(storedBasis);
  const history = useQuotaHistory(request, reloadKey, accounts);
  const forecasts = useMemo(() => {
    const now = Date.now();
    const pools = new Set(accounts.map((a) => a.provider));
    return new Map(
      [...pools].map((p) => [
        p,
        forecast(
          accounts.filter((a) => a.provider === p),
          history,
          basis,
          now,
        ),
      ]),
    );
  }, [accounts, history, basis]);
  const rates = useMemo(
    () =>
      new Map(
        accounts.map((a) => [
          a.id,
          accountRates(a, history, forecasts.get(a.provider) ?? null),
        ]),
      ),
    [accounts, history, forecasts],
  );
  const choose = (next: Basis) => {
    setBasis(next);
    writeStored(basisKey, next);
  };
  return { basis, choose, forecasts, rates };
}
