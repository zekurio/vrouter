import { useEffect, useMemo, useState } from "react";
import type { Account } from "./AccountsPage";
import type { APIRequest } from "./api";
import {
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

// The history of the accounts' headline windows, loaded again whenever the
// state reloads. A failed load keeps the last history; the charts still have
// the current reports.
function useQuotaHistory(
  request: APIRequest,
  reloadKey: string,
  accounts: Account[],
) {
  const [history, setHistory] = useState<QuotaHistory[]>([]);
  const shown = accounts.flatMap((a) => {
    const w = headline(a);
    return w ? [w] : [];
  });
  const windows = [...new Set(shown.map((w) => w.id))].toSorted();
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

// Each pool's forecast by provider, null for a pool without reports, and the
// basis they project from, which the browser remembers.
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
  const choose = (next: Basis) => {
    setBasis(next);
    writeStored(basisKey, next);
  };
  return { basis, choose, forecasts };
}
