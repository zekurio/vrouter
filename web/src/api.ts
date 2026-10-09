// Management API client.

export type APIRequest = <T>(
  path: string,
  method?: string,
  body?: unknown,
) => Promise<T>;
export type AuthMode = "local" | "token" | "external";
export type AuthInfo = { mode: AuthMode; publicUrl: string };
export type Gateway = {
  id: string;
  name: string;
  ownerId: string;
  createdAt: string;
};

export const gatewayHeader = "X-Vrouter-Gateway";

type Options = {
  // Gateway every request is scoped to. Omit for the auth and gateway-list calls.
  gateway?: string;
  // Read on each call. The admin token lives in memory only.
  token: () => string;
  onUnauthorized?: () => void;
  fetch?: typeof fetch;
  timeout?: number;
};

const failure = (message: string, status: number) =>
  Object.assign(new Error(message), { status });
const stale = () =>
  Object.assign(new Error("This gateway is no longer selected."), {
    name: "AbortError",
    status: 0,
  });

export const isStale = (err: unknown) =>
  err instanceof Error && err.name === "AbortError";
export const statusOf = (err: unknown) =>
  err instanceof Error && "status" in err ? Number(err.status) : 0;
export const errorMessage = (err: unknown, fallback: string) =>
  err instanceof Error && err.message ? err.message : fallback;

// Error bodies are {error: "text"} or {error: {type, message, code}}, and a
// stream can send the inner object alone. Returns "" when nothing readable is
// there, so callers can fall back to their own wording.
export function errorText(body: unknown): string {
  if (typeof body === "string") return body.trim().slice(0, 500);
  if (!body || typeof body !== "object") return "";
  const { error, message, type, code } = body as Record<string, unknown>;
  if (typeof error === "string") return errorText(error);
  if (error && typeof error === "object") {
    const inner = errorText(error);
    if (inner) return inner;
  }
  const text = typeof message === "string" ? message.trim().slice(0, 500) : "";
  const tags = [type, code].filter(
    (tag) =>
      (typeof tag === "string" || typeof tag === "number") &&
      tag !== "" &&
      tag !== "error",
  );
  if (!text) return tags.join(", ");
  return tags.length ? `${text} (${tags.join(", ")})` : text;
}

// One client per selected gateway. close() ends it when the selection changes:
// reads in flight are aborted and later reads are refused, so an old gateway's
// data cannot land in the new one's pages. Writes already sent are left to
// finish, and DELETE still goes out after close, because an unmounting provider
// sign-in has to release its session on the gateway that created it.
export function createClient(options: Options) {
  const send = options.fetch ?? ((...args) => fetch(...args));
  const reads = new AbortController();
  let closed = false;

  const request: APIRequest = async (path, method = "GET", body) => {
    const read = method === "GET";
    if (closed && method !== "DELETE") throw stale();
    const token = options.token();
    const timeout = AbortSignal.timeout(options.timeout ?? 15000);
    let response: Response;
    try {
      response = await send(path, {
        method,
        headers: {
          ...(token ? { Authorization: `Bearer ${token}` } : {}),
          ...(options.gateway ? { [gatewayHeader]: options.gateway } : {}),
          ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
        },
        body: body !== undefined ? JSON.stringify(body) : undefined,
        signal: read ? AbortSignal.any([timeout, reads.signal]) : timeout,
      });
    } catch (err) {
      if (closed && read) throw stale();
      if (err instanceof Error && err.name === "TimeoutError")
        throw failure("vrouter did not answer in time.", 0);
      if (err instanceof TypeError)
        throw failure("Could not reach vrouter.", 0);
      throw err;
    }
    const result = await response.json().catch(() => null);
    if (closed && read) throw stale();
    if (response.status === 401 && !closed) options.onUnauthorized?.();
    if (!response.ok)
      throw failure(
        errorText(result) || `Request failed (${response.status}).`,
        response.status,
      );
    return result;
  };

  return {
    request,
    close() {
      closed = true;
      reads.abort();
    },
  };
}

// Keeps a saved selection, otherwise opens the default gateway, then the first.
export function pickGateway(gateways: Gateway[], stored: string | null) {
  const has = (id: string) => gateways.some((g) => g.id === id);
  if (stored && has(stored)) return stored;
  return has("default") ? "default" : (gateways[0]?.id ?? null);
}
