// Gateway API key expiry and status.

export type APIKey = {
  id: string;
  name: string;
  prefix: string;
  createdAt: string;
  revokedAt?: string;
  // Absent means the key never expires.
  expiresAt?: string;
  usedRequests: number;
  usedTokens: number;
};
export type KeyState = "active" | "revoked" | "expired";

// The first reason a call with this key would be refused, if any.
export function keyState(key: APIKey, now = Date.now()): KeyState {
  if (key.revokedAt !== undefined && key.revokedAt !== "") return "revoked";
  if (
    key.expiresAt !== undefined &&
    key.expiresAt !== "" &&
    new Date(key.expiresAt).getTime() <= now
  )
    return "expired";
  return "active";
}

export const stateLabel: Record<KeyState, string> = {
  active: "Active",
  revoked: "Revoked",
  expired: "Expired",
};

const pad = (n: number) => String(n).padStart(2, "0");

// An expiry as the value of a datetime-local input, in the browser's zone.
export function expiryText(expiresAt?: string) {
  if (expiresAt === undefined || expiresAt === "") return "";
  const date = new Date(expiresAt);
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

// Reads the expiry field. Blank means the key never expires, which the API
// writes as null.
export function parseExpiry(
  text: string,
  now = Date.now(),
): { value: string | null } | { error: string } {
  if (!text) return { value: null };
  const date = new Date(text);
  if (Number.isNaN(date.getTime()))
    return { error: "Enter a date and time, or leave it blank." };
  if (date.getTime() <= now) return { error: "Pick a time in the future." };
  return { value: date.toISOString() };
}
