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
  // The account's email or label. Absent when the account is unknown.
  accountName?: string;
  status: number;
  durationMs: number;
  // Time until a streamed response opened its first output block.
  firstOutputMs?: number;
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
  // Why an incomplete request stopped. Older records have none.
  reason?: CutReason;
  // Redacted error summary of a failed request.
  failure?: string;
};
export type CutReason =
  | "output_limit"
  | "content_filter"
  | "provider_stopped"
  | "client_closed"
  | "upstream_ended"
  | "partial_tool"
  | "restart";
const cutReasonLabel: Record<CutReason, string> = {
  output_limit: "Output limit",
  content_filter: "Content filter",
  provider_stopped: "Stopped by provider",
  client_closed: "Client disconnected",
  upstream_ended: "Stream dropped",
  partial_tool: "Partial tool call",
  restart: "Interrupted by restart",
};
export const speeds = ["fast", "ultrafast"] as const;
export type Speed = (typeof speeds)[number];
export const speedLabel: Record<Speed, string> = {
  fast: "Fast",
  ultrafast: "Ultrafast",
};
export type OutcomeFilter = "all" | "error" | "incomplete";

export function formatDuration(ms: number) {
  if (!Number.isFinite(ms) || ms < 0) return "";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)} s`;
  const seconds = Math.round(ms / 1000);
  return `${Math.floor(seconds / 60)}m ${String(seconds % 60).padStart(2, "0")}s`;
}

// The status for a clean call, otherwise the reason it did not finish cleanly.
export function resultLabel(record: RequestRecord) {
  if (record.outcome === "incomplete")
    return record.reason ? cutReasonLabel[record.reason] : "Cut short";
  if (record.outcome === "error") {
    // The status went out before the response broke.
    if (record.status >= 200 && record.status < 300)
      return record.stream ? "Failed mid-stream" : "Failed mid-response";
    return record.status ? `${record.status} error` : "Failed";
  }
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
