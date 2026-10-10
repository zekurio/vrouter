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

// Small output cap used by the connection test and the curl examples.
// Messages requires max_tokens, so those requests never omit it.
const defaultMaxOutput = 256;

export function requestBody(
  protocol: Protocol,
  model: string,
  prompt: string,
  maxOutput?: number,
) {
  const messages = [{ role: "user", content: prompt }];
  if (protocol === "messages")
    return {
      model,
      max_tokens: maxOutput ?? defaultMaxOutput,
      messages,
      stream: true,
    };
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
export function curlExample(base: string, protocol: Protocol, model: string) {
  const headers = Object.entries(authHeaders(protocol, "$VROUTER_API_KEY"))
    .map(([name, value]) => `-H "${name}: ${value}"`)
    .join(" \\\n  ");
  const cap = defaultMaxOutput;
  const body = JSON.stringify(
    requestBody(protocol, model, "Hello", cap),
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
  // Client setting names the provider did not use, from the
  // X-Vrouter-Ignored-Parameters response header. Empty when there are none.
  ignored: string[];
};

const testPrompt = "Reply with OK.";
const testTimeout = 60000;
const maxText = 2000;
const maxErrorBody = 8192;
const maxEvent = 256 * 1024;
const maxStream = 2 * 1024 * 1024;

// One property of a parsed JSON value, or undefined when it has none.
function field(value: unknown, key: string): unknown {
  if (typeof value !== "object" || value === null) return undefined;
  return Object.getOwnPropertyDescriptor(value, key)?.value;
}

type Progress = {
  text: string;
  // Set once the protocol's terminal event has arrived.
  done?: "passed" | "incomplete" | "failed";
  // The model stopped early. Reported once the terminal event arrives.
  early?: boolean;
  detail: string;
};

const finish = (progress: Progress) => {
  progress.done = progress.early === true ? "incomplete" : "passed";
};

const addText = (progress: Progress, text: unknown) => {
  if (typeof text === "string" && progress.text.length < maxText)
    progress.text = (progress.text + text).slice(0, maxText);
};

const fail = (progress: Progress, body: unknown) => {
  progress.done = "failed";
  progress.detail = errorText(body) || "The provider reported an error.";
};

function stopEarly(progress: Progress, reason: unknown, cap?: number) {
  progress.early = true;
  progress.detail =
    reason === "max_output_tokens" ||
    reason === "max_tokens" ||
    reason === "length"
      ? cap === undefined || cap === 0
        ? "The model reached the provider's output limit before it finished."
        : `The model reached the ${cap} token cap of this test before it finished.`
      : `The model stopped early (${typeof reason === "string" && reason !== "" ? reason : "no reason given"}).`;
}

function readResponsesEvent(
  type: unknown,
  event: object,
  progress: Progress,
  cap?: number,
) {
  if (type === "response.output_text.delta")
    addText(progress, field(event, "delta"));
  else if (type === "response.completed") finish(progress);
  else if (type === "response.incomplete") {
    const response = field(event, "response");
    stopEarly(
      progress,
      field(field(response, "incomplete_details"), "reason"),
      cap,
    );
    finish(progress);
  } else if (type === "response.failed")
    fail(progress, field(event, "response") ?? event);
}

function readMessagesEvent(
  type: unknown,
  event: object,
  progress: Progress,
  cap?: number,
) {
  const delta = field(event, "delta");
  if (type === "content_block_delta") addText(progress, field(delta, "text"));
  else if (type === "message_delta") {
    const reason = field(delta, "stop_reason");
    if (Boolean(reason) && reason !== "end_turn" && reason !== "stop_sequence")
      stopEarly(progress, reason, cap);
  } else if (type === "message_stop") finish(progress);
}

function readChatEvent(event: object, progress: Progress, cap?: number) {
  const choice = field(field(event, "choices"), "0");
  addText(progress, field(field(choice, "delta"), "content"));
  const reason = field(choice, "finish_reason");
  if (Boolean(reason) && reason !== "stop") stopEarly(progress, reason, cap);
}

// Applies one SSE event to the test's progress.
function readEvent(
  protocol: Protocol,
  data: string,
  progress: Progress,
  cap?: number,
) {
  if (protocol === "chat" && data === "[DONE]") {
    finish(progress);
    return;
  }
  let event: unknown;
  try {
    event = JSON.parse(data);
  } catch {
    return;
  }
  if (typeof event !== "object" || event === null) return;
  const type = field(event, "type");
  const untyped = type === undefined || type === null || type === "";
  // Every protocol can report a failure in the middle of a 200 stream.
  if (type === "error" || (Boolean(field(event, "error")) && untyped)) {
    fail(progress, event);
    return;
  }

  if (protocol === "responses") readResponsesEvent(type, event, progress, cap);
  else if (protocol === "messages")
    readMessagesEvent(type, event, progress, cap);
  else readChatEvent(event, progress, cap);
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
      // oxlint-disable-next-line eslint/no-await-in-loop -- a stream is read one chunk at a time
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

// What a connection test reads its stream with.
type Reading = {
  protocol: Protocol;
  progress: Progress;
  cap: number;
  onText?: ((text: string) => void) | undefined;
};

// Applies every complete event in buffer and returns the rest.
function readEvents(buffer: string, reading: Reading) {
  const { progress } = reading;
  let rest = buffer;
  let end: number;
  while (progress.done === undefined && (end = rest.indexOf("\n\n")) >= 0) {
    const data = rest
      .slice(0, end)
      .split("\n")
      .filter((line) => line.startsWith("data:"))
      .map((line) => line.slice(5).trimStart())
      .join("\n");
    rest = rest.slice(end + 2);
    if (!data) continue;
    const before = progress.text;
    readEvent(reading.protocol, data, progress, reading.cap);
    if (progress.text !== before) reading.onText?.(progress.text);
  }
  return rest;
}

// Reads the event stream until the terminal event or its end. Returns why the
// stream was given up on, or "" when it was read.
async function readStream(body: ReadableStream<Uint8Array>, reading: Reading) {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let received = 0;
  try {
    while (reading.progress.done === undefined) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- a stream is read one chunk at a time
      const { done, value } = await reader.read();
      if (done) break;
      received += value.byteLength;
      if (received > maxStream)
        return "The response was too large for this test.";
      buffer += decoder.decode(value, { stream: true }).replaceAll("\r", "");
      buffer = readEvents(buffer, reading);
      if (buffer.length > maxEvent)
        return "The stream sent an event that is too big.";
    }
  } finally {
    void reader.cancel().catch(() => {});
  }
  return "";
}

// The detail and request ID from the body of a failed response.
async function failureBody(response: Response) {
  const raw = (await readBounded(response, maxErrorBody)).trim();
  let body: unknown = raw;
  try {
    body = JSON.parse(raw);
  } catch {
    // Not JSON. Show the text itself.
  }
  const id = field(body, "request_id");
  return {
    detail:
      errorText(body) || `The request failed with HTTP ${response.status}.`,
    requestId: typeof id === "string" ? id : "",
  };
}

const ignoredParameters = (response: Response) =>
  (response.headers.get("x-vrouter-ignored-parameters") ?? "")
    .split(",")
    .map((name) => name.trim())
    .filter(Boolean);

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
  const outputCap = defaultMaxOutput;
  const started = performance.now();
  const timeout = AbortSignal.timeout(testTimeout);
  let status = 0;
  let requestId = "";
  let ignored: string[] = [];
  const progress: Progress = { text: "", detail: "" };
  const outcome = (state: TestOutcome["state"], detail: string) => ({
    state,
    status,
    ms: Math.round(performance.now() - started),
    text: progress.text,
    detail,
    requestId,
    ignored,
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
    const header = (name: string) => response.headers.get(name) ?? "";
    requestId = header("x-request-id") || header("request-id");
    ignored = ignoredParameters(response);

    if (!response.ok) {
      const failed = await failureBody(response);
      requestId ||= failed.requestId;
      return outcome("failed", failed.detail);
    }
    const type = header("content-type");
    if (!type.includes("text/event-stream") || !response.body) {
      void response.body?.cancel().catch(() => {});
      return outcome(
        "failed",
        `Expected an event stream, but the response was ${type || "untyped"}.`,
      );
    }

    const stopped = await readStream(response.body, {
      protocol,
      progress,
      cap: outputCap,
      onText: options.onText,
    });
    if (stopped) return outcome("failed", stopped);
    if (progress.done === undefined)
      return outcome(
        "failed",
        "The stream closed before the provider finished the response.",
      );
    return outcome(progress.done, progress.detail);
  } catch {
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
