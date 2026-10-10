// Request telemetry shapes and formatting.

export type RequestRecord = {
  id: string;
  startedAt: string;
  gatewayId: string;
  keyId: string;
  keyName: string;
  model: string;
  nativeModel: string;
  provider: string;
  accountId: string;
  status: number;
  durationMs: number;
  inputTokens: number;
  outputTokens: number;
  // Cache reads and cache writes are both subsets of inputTokens.
  cachedTokens: number;
  cacheWriteTokens?: number;
  totalTokens: number;
  // False when the provider reported no usage. The token fields are then
  // placeholders, not a measured zero.
  usageKnown: boolean;
  usagePartial?: boolean;
  // The premium tier the provider served. Absent means the standard rate.
  speed?: Speed;
  stream: boolean;
  outcome: "success" | "error" | "incomplete";
};
export type Speed = "fast" | "ultrafast";
export const speedLabel: Record<Speed, string> = {
  fast: "Fast",
  ultrafast: "Ultrafast",
};
export type Telemetry = {
  requests: RequestRecord[];
  totals: {
    requests: number;
    inputTokens: number;
    outputTokens: number;
    totalTokens: number;
  };
  retentionLimit: number;
};
export type OutcomeFilter = "all" | "error" | "incomplete";

export function formatDuration(ms: number) {
  if (!Number.isFinite(ms) || ms < 0) return "";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)} s`;
  const seconds = Math.round(ms / 1000);
  return `${Math.floor(seconds / 60)}m ${String(seconds % 60).padStart(2, "0")}s`;
}

// "200" for a clean call, otherwise the reason it did not finish cleanly.
export function resultLabel(record: RequestRecord) {
  if (record.outcome === "incomplete")
    return record.status ? `${record.status}, cut short` : "Cut short";
  if (record.outcome === "error")
    return record.status ? `${record.status} error` : "Failed";
  return record.status ? String(record.status) : "Done";
}

export const keyLabel = (record: RequestRecord) =>
  record.keyName || (record.keyId ? "Unnamed key" : "Server key");

export function filterRequests(
  requests: RequestRecord[],
  outcome: OutcomeFilter,
  key: string,
) {
  return requests.filter(
    (r) =>
      (outcome === "all" || r.outcome === outcome) &&
      (!key || keyLabel(r) === key),
  );
}
