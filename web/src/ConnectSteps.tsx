import { ExternalLink, RefreshCw } from "lucide-react";
import type { Connection } from "./ConnectDialog";

// The pasted callback for a sign-in that cannot reach vrouter on its own.
export type CallbackField = {
  value: string;
  error: string;
  submitting: boolean;
  submitted: boolean;
  onChange: (value: string) => void;
  onSubmit: () => void;
};

const countdown = (session: Connection | null, now: number) => {
  const left = session
    ? Math.max(0, new Date(session.expiresAt).getTime() - now)
    : 0;
  return `${Math.floor(left / 60000)}:${String(
    Math.floor(left / 1000) % 60,
  ).padStart(2, "0")}`;
};

// The steps of a sign-in that is starting or waiting for the provider.
export function ConnectSteps({
  name,
  session,
  ready,
  now,
  error,
  callback,
}: {
  name: string;
  session: Connection | null;
  // Only a live session may be linked. Controls without one keep their space.
  ready: Connection | null;
  now: number;
  error: string;
  callback: CallbackField;
}) {
  const manualCode = session?.flow === "code";
  const deviceCode = session?.flow === "device";
  const waiting = callback.submitted
    ? "Finishing sign-in."
    : "Waiting for sign-in.";
  return (
    <>
      <ol className="connect-steps">
        <OpenStep name={name} ready={ready} deviceCode={deviceCode} />
        <li>
          <h3>Approve access</h3>
          {manualCode && (
            <p>
              Copy the authorization code shown by Claude and paste it below.
            </p>
          )}
          {deviceCode && (
            <p>This dialog finishes automatically after you approve.</p>
          )}
          <p
            className={ready ? "connect-wait" : "connect-wait unavailable"}
            role={ready ? "status" : undefined}
            inert={!ready}
          >
            <RefreshCw size={13} className="spinning" />
            {waiting} Link expires in {countdown(session, now)}.
          </p>
        </li>
      </ol>
      {error && (
        <div className="notice error" role="alert">
          {error}
        </div>
      )}
      {!deviceCode && (
        <ManualCallback
          ready={ready}
          manualCode={manualCode}
          callback={callback}
        />
      )}
    </>
  );
}

// The first step: the link to the provider's sign-in page.
function OpenStep({
  name,
  ready,
  deviceCode,
}: {
  name: string;
  ready: Connection | null;
  deviceCode: boolean;
}) {
  const action = `Open ${name} sign-in`;
  const userCode = ready?.userCode ?? "";
  return (
    <li>
      <h3>Sign in with {name}</h3>
      <p>
        {deviceCode
          ? "Open sign-in and enter this one-time code."
          : "Opens in a new tab."}
      </p>
      {deviceCode && userCode && (
        <div className="copy-field">
          <code>{userCode}</code>
        </div>
      )}
      {ready ? (
        <a
          className="primary"
          href={ready.url}
          target="_blank"
          rel="noopener noreferrer"
        >
          {action} <ExternalLink size={14} />
        </a>
      ) : (
        <button className="primary" disabled>
          {action} <ExternalLink size={14} />
        </button>
      )}
    </li>
  );
}

const callbackPlaceholder = (manualCode: boolean, ready: Connection | null) =>
  manualCode
    ? "code#state"
    : `${(ready?.redirectUri ?? "") || "http://localhost/…"}?code=…&state=…`;

function ManualCallback({
  ready,
  manualCode,
  callback,
}: {
  ready: Connection | null;
  manualCode: boolean;
  callback: CallbackField;
}) {
  const { submitting, submitted } = callback;
  return (
    <section
      className={ready ? "connect-manual" : "connect-manual unavailable"}
      inert={!ready}
      aria-labelledby="callback-label"
    >
      <label id="callback-label" htmlFor="callback-url">
        {manualCode ? "Paste the authorization code" : "Paste the return URL"}
      </label>
      <p id="callback-help">
        {manualCode
          ? "Paste the complete code, including # and the text after it."
          : "If sign-in doesn't finish here, paste the sign-in tab's full URL. This works even if that page can't load."}
      </p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          callback.onSubmit();
        }}
      >
        <input
          id="callback-url"
          type={manualCode ? "text" : "url"}
          value={callback.value}
          onChange={(e) => callback.onChange(e.target.value)}
          placeholder={callbackPlaceholder(manualCode, ready)}
          aria-describedby={
            callback.error ? "callback-help callback-error" : "callback-help"
          }
          aria-invalid={!!callback.error}
          required
          autoComplete="off"
          spellCheck={false}
          disabled={submitting || submitted}
        />
        <button
          className="secondary"
          type="submit"
          disabled={submitting || submitted || !callback.value.trim()}
        >
          {submitted
            ? "Accepted"
            : submitting
              ? "Connecting…"
              : "Finish sign-in"}
        </button>
      </form>
      {callback.error && (
        <p id="callback-error" className="connect-url-error" role="alert">
          {callback.error}
        </p>
      )}
    </section>
  );
}
