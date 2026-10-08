// The client protocols vrouter serves under /v1, with one request builder and
// one stream reader shared by the curl examples and the connection test.

import { errorText } from "./api";

export type Protocol = "responses" | "messages" | "chat";
export const protocols: { value: Protocol; label: string; path: string }[] = [
  { value: "responses", label: "Responses", path: "/responses" },
  { value: "messages", label: "Messages", path: "/messages" },
  { value: "chat", label: "Chat Completions", path: "/chat/completions" },
];
export const protocolPath = (protocol: Protocol) =>
  protocols.find((p) => p.value === protocol)!.path;

const anthropicVersion = "2023-06-01";

export function requestBody(
  protocol: Protocol,
  model: string,
  prompt: string,
  maxOutput?: number,
) {
  const messages = [{ role: "user", content: prompt }];
  if (protocol === "messages")
    return { model, max_tokens: maxOutput, messages, stream: true };
  if (protocol === "chat")
    return {
      model,
      messages,
      max_completion_tokens: maxOutput,
      stream: true,
    };
  return {
    model,
    input: messages,
    max_output_tokens: maxOutput,
    stream: true,
    store: false,
  };
}

function authHeaders(protocol: Protocol, key: string): Record<string, string> {
  return protocol === "messages"
    ? { "x-api-key": key, "anthropic-version": anthropicVersion }
    : { Authorization: `Bearer ${key}` };
}

const shellQuote = (value: string) => `'${value.replaceAll("'", "'\\''")}'`;

// base is the public address ending in /v1.
export function curlExample(
  base: string,
  protocol: Protocol,
  model: string,
  provider: string,
) {
  const headers = Object.entries(authHeaders(protocol, "$VROUTER_API_KEY"))
    .map(([name, value]) => `-H "${name}: ${value}"`)
    .join(" \\\n  ");
  const body = JSON.stringify(
    requestBody(
      protocol,
      model,
      "Hello",
      provider.toLowerCase() === "codex" ? undefined : 1024,
    ),
    null,
    2,
  );
  return `curl --no-buffer ${shellQuote(base + protocolPath(protocol))} \\\n  ${headers} \\\n  -H "Content-Type: application/json" \\\n  -d ${shellQuote(body)}`;
}

export type TestOutcome = {
  // passed needs the protocol's terminal event. incomplete means the stream
  // ended properly but the model stopped early, usually at the output cap.
  state: "passed" | "incomplete" | "failed" | "cancelled";
  // HTTP status, or 0 when no response arrived.
  status: number;
  ms: number;
  text: string;
  detail: string;
  requestId: string;
};

const testPrompt = "Reply with OK.";
const testMaxOutput = 256;
const testTimeout = 60000;
const maxText = 2000;
const maxErrorBody = 8192;
const maxEvent = 256 * 1024;
const maxStream = 2 * 1024 * 1024;

type Json = Record<string, any>;
type Progress = {
  text: string;
  // Set once the protocol's terminal event has arrived.
  done?: "passed" | "incomplete" | "failed";
  // The model stopped early. Reported once the terminal event arrives.
  early?: boolean;
  detail: string;
};

// Applies one SSE event to the test's progress.
function readEvent(
  protocol: Protocol,
  data: string,
  progress: Progress,
  cap?: number,
) {
  const finish = () => {
    progress.done = progress.early ? "incomplete" : "passed";
  };
  if (protocol === "chat" && data === "[DONE]") return finish();
  let event: Json;
  try {
    event = JSON.parse(data);
  } catch {
    return;
  }
  if (!event || typeof event !== "object") return;
  const add = (text: unknown) => {
    if (typeof text === "string" && progress.text.length < maxText)
      progress.text = (progress.text + text).slice(0, maxText);
  };
  const fail = (body: unknown) => {
    progress.done = "failed";
    progress.detail = errorText(body) || "The provider reported an error.";
  };
  const stopEarly = (reason: unknown) => {
    progress.early = true;
    progress.detail =
      reason === "max_output_tokens" ||
      reason === "max_tokens" ||
      reason === "length"
        ? cap
          ? `The model reached the ${cap} token cap of this test before it finished.`
          : "The model reached the provider's output limit before it finished."
        : `The model stopped early (${String(reason || "no reason given")}).`;
  };
  // Every protocol can report a failure in the middle of a 200 stream.
  if (event.type === "error" || (event.error && !event.type))
    return fail(event);

  if (protocol === "responses") {
    if (event.type === "response.output_text.delta") add(event.delta);
    else if (event.type === "response.completed") finish();
    else if (event.type === "response.incomplete") {
      stopEarly(event.response?.incomplete_details?.reason);
      finish();
    } else if (event.type === "response.failed") fail(event.response ?? event);
  } else if (protocol === "messages") {
    if (event.type === "content_block_delta") add(event.delta?.text);
    else if (event.type === "message_delta") {
      const reason = event.delta?.stop_reason;
      if (reason && reason !== "end_turn" && reason !== "stop_sequence")
        stopEarly(reason);
    } else if (event.type === "message_stop") finish();
  } else {
    const choice = event.choices?.[0];
    add(choice?.delta?.content);
    const reason = choice?.finish_reason;
    if (reason && reason !== "stop") stopEarly(reason);
  }
}

// Reads at most limit bytes of a response body as text.
async function readBounded(response: Response, limit: number) {
  const reader = response.body?.getReader();
  if (!reader) return "";
  const decoder = new TextDecoder();
  let text = "";
  let received = 0;
  try {
    while (received < limit) {
      const { done, value } = await reader.read();
      if (done) break;
      const part = value.subarray(0, limit - received);
      received += part.byteLength;
      text += decoder.decode(part, { stream: true });
    }
    text += decoder.decode();
  } finally {
    void reader.cancel().catch(() => {});
  }
  return text.slice(0, limit);
}

// Sends one small streaming request to the public endpoint, authenticated only
// by the given inference key, and resolves once the stream ends. The key is
// used for this call and kept nowhere.
export async function runConnectionTest(options: {
  // Public address ending in /v1.
  base: string;
  protocol: Protocol;
  model: string;
  provider: string;
  key: string;
  // Aborting reports the test as cancelled.
  signal: AbortSignal;
  onText?: (text: string) => void;
}): Promise<TestOutcome> {
  const { protocol, signal } = options;
  const outputCap =
    options.provider.toLowerCase() === "codex" ? undefined : testMaxOutput;
  const started = performance.now();
  const timeout = AbortSignal.timeout(testTimeout);
  let status = 0;
  let requestId = "";
  const progress: Progress = { text: "", detail: "" };
  const outcome = (state: TestOutcome["state"], detail: string) => ({
    state,
    status,
    ms: Math.round(performance.now() - started),
    text: progress.text,
    detail,
    requestId,
  });

  try {
    const response = await fetch(options.base + protocolPath(protocol), {
      method: "POST",
      // Only the key may authorize this call, never a dashboard session.
      credentials: "omit",
      redirect: "error",
      cache: "no-store",
      headers: {
        ...authHeaders(protocol, options.key),
        "Content-Type": "application/json",
        Accept: "text/event-stream",
      },
      body: JSON.stringify(
        requestBody(protocol, options.model, testPrompt, outputCap),
      ),
      signal: AbortSignal.any([signal, timeout]),
    });
    status = response.status;
    requestId =
      response.headers.get("x-request-id") ||
      response.headers.get("request-id") ||
      "";

    if (!response.ok) {
      const raw = (await readBounded(response, maxErrorBody)).trim();
      let body: unknown = raw;
      try {
        body = JSON.parse(raw);
      } catch {
        // Not JSON. Show the text itself.
      }
      const id = (body as Json | null)?.request_id;
      if (typeof id === "string") requestId ||= id;
      return outcome(
        "failed",
        errorText(body) || `The request failed with HTTP ${status}.`,
      );
    }
    const type = response.headers.get("content-type") || "";
    if (!type.includes("text/event-stream") || !response.body) {
      void response.body?.cancel().catch(() => {});
      return outcome(
        "failed",
        `Expected an event stream, but the response was ${type || "untyped"}.`,
      );
    }

    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    let received = 0;
    try {
      while (!progress.done) {
        const { done, value } = await reader.read();
        if (done) break;
        received += value.byteLength;
        if (received > maxStream)
          return outcome("failed", "The response was too large for this test.");
        buffer += decoder.decode(value, { stream: true }).replace(/\r/g, "");
        let end: number;
        while (!progress.done && (end = buffer.indexOf("\n\n")) >= 0) {
          const data = buffer
            .slice(0, end)
            .split("\n")
            .filter((line) => line.startsWith("data:"))
            .map((line) => line.slice(5).trimStart())
            .join("\n");
          buffer = buffer.slice(end + 2);
          if (!data) continue;
          const before = progress.text;
          readEvent(protocol, data, progress, outputCap);
          if (progress.text !== before) options.onText?.(progress.text);
        }
        if (buffer.length > maxEvent)
          return outcome("failed", "The stream sent an event that is too big.");
      }
    } finally {
      void reader.cancel().catch(() => {});
    }
    if (!progress.done)
      return outcome(
        "failed",
        "The stream closed before the provider finished the response.",
      );
    return outcome(progress.done, progress.detail);
  } catch (err) {
    if (signal.aborted) return outcome("cancelled", "");
    if (timeout.aborted)
      return outcome(
        "failed",
        `No complete response within ${testTimeout / 1000} seconds.`,
      );
    return outcome(
      "failed",
      status
        ? "The connection dropped before the response finished."
        : "Could not reach the endpoint.",
    );
  }
}
