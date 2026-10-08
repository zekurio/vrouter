import { useEffect, useRef, useState } from "react";
import {
  protocolPath,
  runConnectionTest,
  type Protocol,
  type TestOutcome,
} from "./protocols";

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
  blocked?: string;
}) {
  const [key, setKey] = useState("");
  const [running, setRunning] = useState(false);
  const [reply, setReply] = useState("");
  const [outcome, setOutcome] = useState<TestOutcome | null>(null);
  // Path of the request on screen. The protocol can change after a run.
  const [path, setPath] = useState("");
  const abort = useRef<AbortController | null>(null);
  useEffect(() => () => abort.current?.abort(), []);

  async function run() {
    const secret = key.trim();
    if (!secret || running || blocked) return;
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

  const shown = running || outcome;
  const facts = outcome && [
    outcome.status ? `HTTP ${outcome.status}` : "",
    `${(outcome.ms / 1000).toFixed(1)}s`,
    outcome.requestId ? `request ${outcome.requestId}` : "",
  ];
  return (
    <section className="connection-test" aria-labelledby="connection-test">
      <h3 id="connection-test">Test this model</h3>
      <p>
        Sends "Reply with OK." to {base}
        {protocolPath(protocol)} with your API key. The request counts against
        that key like any other. The key is not saved.
      </p>
      <div className="connection-test-form">
        <input
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
          <button className="secondary" onClick={() => abort.current?.abort()}>
            Cancel
          </button>
        ) : (
          <button
            className="primary"
            disabled={!key.trim() || !!blocked}
            onClick={() => void run()}
          >
            Run test
          </button>
        )}
      </div>
      {blocked && <p>{blocked}</p>}
      {shown && (
        <div className="connection-test-result" role="status">
          <div className="connection-test-status">
            <strong className={outcome?.state ?? "pending"}>
              {outcome ? labels[outcome.state] : "Waiting for the response"}
            </strong>
            <code>POST {path}</code>
            {facts && <span>{facts.filter(Boolean).join(" · ")}</span>}
          </div>
          {outcome?.detail && <p>{outcome.detail}</p>}
          {outcome && outcome.ignored.length > 0 && (
            <p>Provider does not use: {outcome.ignored.join(", ")}.</p>
          )}
          {reply && <pre tabIndex={0}>{reply}</pre>}
        </div>
      )}
    </section>
  );
}
