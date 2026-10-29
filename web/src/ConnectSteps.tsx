import { Check, ExternalLink, RefreshCw } from "lucide-react";
import type { Connection, Scope } from "./ConnectDialog";
import { button, copyField, cx, dialogActions, dialogLead, notice } from "./ui";

const failure = cx(notice({ error: true, inset: true }), "mt-4.5");

// Each step is numbered in a ring, and a rule joins it to the next. The ring
// is 24px plus its border, as the old content-box pseudo-element was.
const step =
  "relative pb-6 pl-9.5 [counter-increment:step] last:pb-0 before:absolute before:-top-0.75 before:left-0 before:flex before:size-6.5 before:items-center before:justify-center before:rounded-full before:border before:border-line before:text-[11px] before:text-provider before:tabular-nums before:content-[counter(step)] not-last:after:absolute not-last:after:top-6.75 not-last:after:bottom-0.75 not-last:after:left-3 not-last:after:w-px not-last:after:bg-line";
const stepTitle = "text-[13px] font-[550]";
const stepNote = "mt-1.25 text-[12px] leading-[1.6] text-muted";

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
      <ol className="mt-5.5 [counter-reset:step]">
        <OpenStep name={name} ready={ready} deviceCode={deviceCode} />
        <li className={step}>
          <h3 className={stepTitle}>Approve access</h3>
          {manualCode && (
            <p className={stepNote}>
              Copy the authorization code shown by Claude and paste it below.
            </p>
          )}
          {deviceCode && (
            <p className={stepNote}>
              This dialog finishes automatically after you approve.
            </p>
          )}
          {/* Keeps the final layout in place until the session is ready. */}
          <p
            className={cx(
              "mt-2.5 flex items-center gap-2 text-[12px] leading-[1.6] text-subtle tabular-nums",
              !ready && "invisible",
            )}
            role={ready ? "status" : undefined}
            inert={!ready}
          >
            <RefreshCw size={13} className="animate-spin" />
            {waiting} Link expires in {countdown(session, now)}.
          </p>
        </li>
      </ol>
      {error && (
        <div className={failure} role="alert">
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
    <li className={step}>
      <h3 className={stepTitle}>Sign in with {name}</h3>
      <p className={stepNote}>
        {deviceCode
          ? "Open sign-in and enter this one-time code."
          : "Opens in a new tab."}
      </p>
      {deviceCode && userCode && (
        <div className={copyField}>
          <code>{userCode}</code>
        </div>
      )}
      {ready ? (
        <a
          className={cx(button("primary"), "mt-3")}
          href={ready.url}
          target="_blank"
          rel="noopener noreferrer"
        >
          {action} <ExternalLink size={14} />
        </a>
      ) : (
        <button className={cx(button("primary"), "mt-3")} disabled>
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
      className={cx(
        "mt-5.5 border-t border-border pt-4",
        !ready && "invisible",
      )}
      inert={!ready}
      aria-labelledby="callback-label"
    >
      <label
        id="callback-label"
        htmlFor="callback-url"
        className="text-[13px] font-[550]"
      >
        {manualCode ? "Paste the authorization code" : "Paste the return URL"}
      </label>
      <p id="callback-help" className={stepNote}>
        {manualCode
          ? "Paste the complete code, including # and the text after it."
          : "If sign-in doesn't finish here, paste the sign-in tab's full URL. This works even if that page can't load."}
      </p>
      <form
        className="mt-3 flex flex-wrap gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          callback.onSubmit();
        }}
      >
        {/* The placeholder keeps Chrome's own grey, which preflight lightens. */}
        <input
          id="callback-url"
          className="h-8 w-full min-w-[min(100%,240px)] flex-1 rounded-lg border border-line bg-well px-2.5 text-[12px] text-text placeholder:text-[#757575]"
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
          className={button("secondary")}
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
        <p
          id="callback-error"
          className="mt-2.5 text-[12px] leading-[1.6] text-danger"
          role="alert"
        >
          {callback.error}
        </p>
      )}
    </section>
  );
}

// The sign-in finished and the account is stored.
export function ConnectDone({
  name,
  scope,
  onAnother,
  onClose,
}: {
  name: string;
  scope: Scope;
  onAnother: () => void;
  onClose: () => void;
}) {
  return (
    <>
      <p className={cx(dialogLead, "flex items-center gap-2.25")}>
        <Check size={16} className="shrink-0 text-green" /> Added to{" "}
        {scope === "global" ? `the shared ${name}` : `your ${name}`} accounts.
        vrouter hasn&apos;t sent a test request, so confirm with a client call.
      </p>
      <div className={cx(dialogActions, "mt-6")}>
        <button className={button("secondary")} onClick={onAnother}>
          Add another
        </button>
        <button className={button("primary")} onClick={onClose}>
          Done
        </button>
      </div>
    </>
  );
}

// The sign-in failed or expired.
export function ConnectFailed({
  error,
  onClose,
  onRetry,
}: {
  error: string;
  onClose: () => void;
  onRetry: () => void;
}) {
  return (
    <>
      <div className={failure} role="alert">
        {error}
      </div>
      <div className={cx(dialogActions, "mt-6")}>
        <button className={button("secondary")} onClick={onClose}>
          Close
        </button>
        <button className={button("primary")} onClick={onRetry}>
          Start again
        </button>
      </div>
    </>
  );
}
