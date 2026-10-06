// Gateway API key limits and status.

export type APIKey = {
  id: string;
  name: string;
  prefix: string;
  createdAt: string;
  revokedAt?: string;
  // 0 means no limit. Limits are lifetime totals.
  limitRequests: number;
  limitTokens: number;
  usedRequests: number;
  usedTokens: number;
  // Set when a token-limited call finished without a usage report.
  usageUncertain?: boolean;
};
export type KeyState =
  "active" | "revoked" | "uncertain" | "requests-spent" | "tokens-spent";

// The first reason a call with this key would be refused, if any.
export function keyState(key: APIKey): KeyState {
  if (key.revokedAt) return "revoked";
  if (key.usageUncertain && key.limitTokens > 0) return "uncertain";
  if (key.limitRequests > 0 && key.usedRequests >= key.limitRequests)
    return "requests-spent";
  if (key.limitTokens > 0 && key.usedTokens >= key.limitTokens)
    return "tokens-spent";
  return "active";
}

export const stateLabel: Record<KeyState, string> = {
  active: "Active",
  revoked: "Revoked",
  uncertain: "Blocked, usage unknown",
  "requests-spent": "Request limit reached",
  "tokens-spent": "Token limit reached",
};

// Share of a limit used, as a bar width. Token usage can pass its limit because
// vrouter counts tokens after the response, so the bar stops at full.
export const usedShare = (used: number, limit: number) =>
  limit > 0 ? Math.min(100, Math.max(0, (used / limit) * 100)) : 0;

// Reads a limit field. Blank means no limit, which the API writes as 0.
export function parseLimit(
  text: string,
): { value: number } | { error: string } {
  const digits = text.trim().replace(/[\s,_]/g, "");
  if (!digits) return { value: 0 };
  if (!/^\d+$/.test(digits))
    return { error: "Enter a whole number, or leave it blank for no limit." };
  const value = Number(digits);
  if (!Number.isSafeInteger(value))
    return { error: "That number is too large." };
  return { value };
}

export const limitText = (limit: number) => (limit > 0 ? String(limit) : "");
