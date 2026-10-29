import { useEffect, useRef, useState } from "react";
import {
  protocolPath,
  runConnectionTest,
  type Protocol,
  type TestOutcome,
} from "./protocols";
import { button, cx } from "./ui";

const stateColors: Record<string, string> = {
  passed: "text-green",
  incomplete: "text-amber",
  failed: "text-danger",
};
const note = "mt-1.5 text-[11.5px] leading-[1.6] wrap-anywhere";

const labels: Record<TestOutcome["state"], string> = {
  passed: "Passed",
  incomplete: "Incomplete",
  failed: "Failed",
  cancelled: "Cancelled",
};

// Sends a real request through the public endpoint with a key the user types
// in. The key stays in this component's state, so it is gone as soon as the
// dialog closes or the workspace is switched, and a running test is aborted.
export function ConnectionTest({
  base,
  protocol,
  model,
  provider,
  blocked,
}: {
  // Address the test calls, ending in /v1.
  base: string;
  protocol: Protocol;
  model: string;
  provider: string;
  // Why the test cannot run right now, if it cannot.
  blocked?: string | undefined;
}) {
  const [key, setKey] = useState("");
  const [running, setRunning] = useState(false);
  const [reply, setReply] = useState("");
  const [outcome, setOutcome] = useState<TestOutcome | null>(null);
  // Path of the request on screen. The protocol can change after a run.
  const [path, setPath] = useState("");
  const abort = useRef<AbortController | null>(null);
  const isBlocked = blocked !== undefined && blocked !== "";
  useEffect(() => () => abort.current?.abort(), []);

  async function run() {
    const secret = key.trim();
    if (!secret || running || isBlocked) return;
    const controller = new AbortController();
    abort.current = controller;
    setRunning(true);
    setReply("");
    setOutcome(null);
    setPath(`/v1${protocolPath(protocol)}`);
    const result = await runConnectionTest({
      base,
      protocol,
      model,
      provider,
      key: secret,
      signal: controller.signal,
      onText: (text) => {
        if (!controller.signal.aborted) setReply(text);
      },
    });
    if (abort.current !== controller) return;
    abort.current = null;
    setRunning(false);
    setReply(result.text);
    setOutcome(result);
  }

  return (
    <section className="mt-5.5" aria-labelledby="connection-test">
      <h3 id="connection-test" className="text-[13px] font-[550]">
        Test this model
      </h3>
      <p className={cx(note, "text-muted")}>
        Sends &quot;Reply with OK.&quot; to {base}
        {protocolPath(protocol)} with your API key. The request counts against
        that key like any other. The key is not saved.
      </p>
      <div className="mt-3 flex gap-2">
        <input
          className="h-8 min-w-0 flex-1 rounded-lg border border-line bg-well px-2.5 text-[13px] text-text placeholder:text-muted focus:border-accent focus:outline-none"
          type="password"
          aria-label="API key for the test"
          placeholder="API key"
          value={key}
          autoComplete="off"
          spellCheck={false}
          disabled={running}
          onChange={(e) => setKey(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") void run();
          }}
        />
        {running ? (
          <button
            className={button("secondary")}
            onClick={() => abort.current?.abort()}
          >
            Cancel
          </button>
        ) : (
          <button
            className={button("primary")}
            disabled={!key.trim() || isBlocked}
            onClick={() => void run()}
          >
            Run test
          </button>
        )}
      </div>
      {isBlocked && <p className={cx(note, "text-muted")}>{blocked}</p>}
      {(running || outcome !== null) && (
        <TestResult outcome={outcome} path={path} reply={reply} />
      )}
    </section>
  );
}

// The status, details, and reply of the test that is running or last ran.
function TestResult({
  outcome,
  path,
  reply,
}: {
  outcome: TestOutcome | null;
  path: string;
  reply: string;
}) {
  const facts = outcome && [
    outcome.status ? `HTTP ${outcome.status}` : "",
    `${(outcome.ms / 1000).toFixed(1)}s`,
    outcome.requestId ? `request ${outcome.requestId}` : "",
  ];
  return (
    <div
      className="mt-3 rounded-lg border border-line bg-well px-3 py-2.5"
      role="status"
    >
      <div className="flex flex-wrap items-baseline gap-x-2.5 gap-y-1 text-[12px]">
        <strong
          className={cx(
            "font-[550]",
            stateColors[outcome?.state ?? ""] ?? "text-subtle",
          )}
        >
          {outcome ? labels[outcome.state] : "Waiting for the response"}
        </strong>
        <code className="text-subtle">POST {path}</code>
        {facts && (
          <span className="ml-auto text-[11px] text-muted">
            {facts.filter(Boolean).join(" · ")}
          </span>
        )}
      </div>
      {outcome !== null && outcome.detail !== "" && (
        <p className={cx(note, "text-subtle")}>{outcome.detail}</p>
      )}
      {outcome && outcome.ignored.length > 0 && (
        <p className={cx(note, "text-subtle")}>
          Provider does not use: {outcome.ignored.join(", ")}.
        </p>
      )}
      {reply && (
        <pre
          className="mt-2 max-h-30 overflow-auto text-[12px] leading-[1.6] whitespace-pre-wrap text-text wrap-anywhere"
          tabIndex={0}
        >
          {reply}
        </pre>
      )}
    </div>
  );
}
