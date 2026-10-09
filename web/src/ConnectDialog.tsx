import { useEffect, useRef, useState, type CSSProperties } from "react";
import { Check, ExternalLink, RefreshCw, X } from "lucide-react";
import { errorMessage, isStale, statusOf, type APIRequest } from "./api";
import { ProviderBrand, providerColor, providerLabel } from "./ProviderBrand";

// Providers vrouter can sign in to. Codex is the pool for OpenAI sign-in.
export const connectable = ["codex", "claude"];

type Connection = {
  id: string;
  provider: string;
  url: string;
  expiresAt: string;
  redirectUri?: string;
  flow: "loopback" | "code" | "device";
  userCode?: string;
};
type Props = {
  // "" shows the provider choice; a provider ID starts sign-in right away.
  provider: string;
  // Set when renewing a saved account instead of registering a new one.
  accountId?: string;
  request: APIRequest;
  choose: (provider: string) => void;
  onClose: () => void;
  onConnected: () => void;
};

export function ConnectDialog({
  provider,
  accountId,
  request,
  choose,
  onClose,
  onConnected,
}: Props) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [session, setSession] = useState<Connection | null>(null);
  const [phase, setPhase] = useState<
    "starting" | "pending" | "connected" | "error"
  >("starting");
  const [error, setError] = useState("");
  const [callback, setCallback] = useState("");
  const [callbackError, setCallbackError] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [submitted, setSubmitted] = useState(false);
  const [now, setNow] = useState(Date.now());
  const [attempt, setAttempt] = useState(0);
  // Reconnect applies to the first attempt and retries. Add another starts fresh.
  const [target, setTarget] = useState(accountId || "");
  const onConnectedRef = useRef(onConnected);
  onConnectedRef.current = onConnected;
  const live = useRef<Connection | null>(null);
  const id = connectable.includes(provider) ? provider : "";
  const name = id && providerLabel(id);
  // Only a live session may be linked. Controls without one keep their space.
  const ready = phase === "pending" ? session : null;
  const manualCode = session?.flow === "code";
  const deviceCode = session?.flow === "device";
  const action = `Open ${name} sign-in`;

  // Clears the old session in the same render so its link cannot be clicked.
  function restart(next: string) {
    setSession(null);
    setPhase("starting");
    setError("");
    setCallback("");
    setCallbackError("");
    setSubmitted(false);
    setTarget(next);
    setAttempt((n) => n + 1);
  }

  useEffect(() => dialog.current?.showModal(), []);
  // Frees a sign-in the server still holds. DELETE goes out even after the
  // gateway's client has closed.
  const cancel = (id: string) =>
    void request(`/api/oauth/sessions/${id}`, "DELETE").catch(() => {});
  // Leaving the dialog or starting over releases the session.
  const release = () => {
    const pending = live.current;
    live.current = null;
    if (pending && new Date(pending.expiresAt).getTime() > Date.now())
      cancel(pending.id);
  };

  useEffect(() => {
    if (!provider) return;
    let stopped = false;
    request<Connection>(
      `/api/oauth/${provider}`,
      "POST",
      target ? { accountId: target } : undefined,
    )
      .then((next) => {
        if (stopped) return cancel(next.id);
        live.current = next;
        setNow(Date.now());
        setSession(next);
        setPhase("pending");
      })
      .catch((err) => {
        if (stopped) return;
        setPhase("error");
        setError(errorMessage(err, "Could not start sign-in."));
      });
    return () => {
      stopped = true;
      release();
    };
  }, [provider, attempt, target, request]);

  useEffect(() => {
    if (!session || phase !== "pending") return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      if (new Date(session.expiresAt).getTime() <= Date.now()) {
        live.current = null;
        setPhase("error");
        setError("The sign-in link expired before the account was connected.");
        return;
      }
      try {
        const result = await request<{ status: string; error?: string }>(
          `/api/oauth/sessions/${session.id}`,
        );
        if (stopped) return;
        if (result.status === "connected") {
          live.current = null;
          setPhase("connected");
          setError("");
          onConnectedRef.current();
          return;
        }
        if (result.status === "error") {
          setPhase("error");
          setError(result.error || "Provider sign-in failed.");
          return;
        }
        setError("");
        timer = setTimeout(poll, 2500);
      } catch (err) {
        if (stopped || isStale(err)) return;
        // The server dropped the session, so waiting longer cannot help.
        if (statusOf(err) === 410) {
          live.current = null;
          setPhase("error");
          setError(errorMessage(err, "Sign-in expired."));
          return;
        }
        setError(errorMessage(err, "Could not check sign-in."));
        timer = setTimeout(poll, 5000);
      }
    };
    timer = setTimeout(poll, 1500);
    const clock = setInterval(() => setNow(Date.now()), 1000);
    return () => {
      stopped = true;
      clearTimeout(timer);
      clearInterval(clock);
    };
  }, [session, phase, request]);

  async function submit() {
    if (!ready || submitting || submitted || !callback.trim()) return;
    setSubmitting(true);
    setCallbackError("");
    try {
      await request(
        `/api/oauth/sessions/${ready.id}/callback`,
        "POST",
        manualCode
          ? { code: callback.trim() }
          : { redirectUrl: callback.trim() },
      );
      setCallback("");
      setSubmitted(true);
    } catch (err) {
      setCallbackError(
        errorMessage(
          err,
          "Could not finish sign-in. Check what you pasted and try again.",
        ),
      );
    } finally {
      setSubmitting(false);
    }
  }
  const left = session
    ? Math.max(0, new Date(session.expiresAt).getTime() - now)
    : 0;
  const countdown = `${Math.floor(left / 60000)}:${String(
    Math.floor(left / 1000) % 60,
  ).padStart(2, "0")}`;

  return (
    <dialog
      ref={dialog}
      className="connect-dialog"
      style={{ "--provider": providerColor(id) } as CSSProperties}
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
      aria-labelledby="connect-title"
    >
      <div className="dialog-heading">
        <h2 id="connect-title">
          {phase === "connected" && name
            ? target
              ? `${name} account reconnected`
              : `${name} sign-in saved`
            : name
              ? target
                ? `Reconnect ${name} account`
                : `Add a ${name} account`
              : "Add an account"}
        </h2>
        {phase !== "connected" && phase !== "error" && (
          <button
            className="icon-button"
            aria-label={name ? "Cancel sign-in" : "Close dialog"}
            onClick={onClose}
          >
            <X size={18} />
          </button>
        )}
      </div>
      {!name ? (
        <>
          <p className="dialog-lead">
            Tokens stay on the server and never pass through this browser.
          </p>
          <div className="provider-choice">
            {connectable.map((p) => (
              <button
                key={p}
                style={{ "--provider": providerColor(p) } as CSSProperties}
                onClick={() => choose(p)}
              >
                <ProviderBrand provider={p} />
                <span>
                  <strong>Sign in with {providerLabel(p)}</strong>
                  {p === "codex"
                    ? "ChatGPT subscription"
                    : "Claude subscription"}
                </span>
              </button>
            ))}
          </div>
        </>
      ) : phase === "connected" ? (
        <>
          <p className="dialog-lead connect-done">
            <Check size={16} /> Added to the {name} pool. vrouter hasn't sent a
            test request, so confirm with a client call.
          </p>
          <div className="dialog-actions">
            <button className="secondary" onClick={() => restart("")}>
              Add another
            </button>
            <button className="primary" onClick={onClose}>
              Done
            </button>
          </div>
        </>
      ) : phase === "error" ? (
        <>
          <div className="notice error" role="alert">
            {error}
          </div>
          <div className="dialog-actions">
            <button className="secondary" onClick={onClose}>
              Close
            </button>
            <button className="primary" onClick={() => restart(target)}>
              Start again
            </button>
          </div>
        </>
      ) : (
        <>
          <ol className="connect-steps">
            <li>
              <h3>Sign in with {name}</h3>
              <p>
                {deviceCode
                  ? "Open sign-in and enter this one-time code."
                  : "Opens in a new tab."}
              </p>
              {deviceCode && ready?.userCode && (
                <div className="copy-field">
                  <code>{ready.userCode}</code>
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
            <li>
              <h3>Approve access</h3>
              {manualCode && (
                <p>
                  Copy the authorization code shown by Claude and paste it
                  below.
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
                {submitted ? "Finishing sign-in." : "Waiting for sign-in."} Link
                expires in {countdown}.
              </p>
            </li>
          </ol>
          {error && (
            <div className="notice error" role="alert">
              {error}
            </div>
          )}
          {!deviceCode && (
            <section
              className={
                ready ? "connect-manual" : "connect-manual unavailable"
              }
              inert={!ready}
              aria-labelledby="callback-label"
            >
              <label id="callback-label" htmlFor="callback-url">
                {manualCode
                  ? "Paste the authorization code"
                  : "Paste the return URL"}
              </label>
              <p id="callback-help">
                {manualCode
                  ? "Paste the complete code, including # and the text after it."
                  : "If sign-in doesn't finish here, paste the sign-in tab's full URL. This works even if that page can't load."}
              </p>
              <form
                onSubmit={(e) => {
                  e.preventDefault();
                  void submit();
                }}
              >
                <input
                  id="callback-url"
                  type={manualCode ? "text" : "url"}
                  value={callback}
                  onChange={(e) => {
                    setCallback(e.target.value);
                    setCallbackError("");
                  }}
                  placeholder={
                    manualCode
                      ? "code#state"
                      : `${ready?.redirectUri || "http://localhost/…"}?code=…&state=…`
                  }
                  aria-describedby={
                    callbackError
                      ? "callback-help callback-error"
                      : "callback-help"
                  }
                  aria-invalid={!!callbackError}
                  required
                  autoComplete="off"
                  spellCheck={false}
                  disabled={submitting || submitted}
                />
                <button
                  className="secondary"
                  type="submit"
                  disabled={submitting || submitted || !callback.trim()}
                >
                  {submitted
                    ? "Accepted"
                    : submitting
                      ? "Connecting…"
                      : "Finish sign-in"}
                </button>
              </form>
              {callbackError && (
                <p
                  id="callback-error"
                  className="connect-url-error"
                  role="alert"
                >
                  {callbackError}
                </p>
              )}
            </section>
          )}
        </>
      )}
    </dialog>
  );
}
