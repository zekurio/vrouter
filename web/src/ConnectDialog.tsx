import { useEffect, useRef, useState, type CSSProperties } from "react";
import { Check, ExternalLink, RefreshCw, X } from "lucide-react";
import type { APIRequest } from "./AccountsPage";
import { ProviderBrand, providerColor } from "./ProviderBrand";

// Providers vrouter can sign in to. Codex is the pool for OpenAI sign-in.
export const connectable = ["Codex", "Claude"];

type Connection = {
  id: string;
  provider: string;
  url: string;
  expiresAt: string;
  redirectUri?: string;
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
const message = (err: unknown, fallback: string) =>
  err instanceof Error ? err.message : fallback;

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
  const name = connectable.find((p) => p.toLowerCase() === provider) || "";
  // Only a live session may be linked. Controls without one keep their space.
  const ready = phase === "pending" ? session : null;
  const action = `Open ${name} sign-in`;

  // Clear the old session in the same render so its link cannot be clicked.
  function clear() {
    setSession(null);
    setPhase("starting");
    setError("");
    setCallback("");
    setCallbackError("");
    setSubmitted(false);
  }
  function restart(next: string) {
    clear();
    setTarget(next);
    setAttempt((n) => n + 1);
  }

  useEffect(() => dialog.current?.showModal(), []);
  // Leaving the dialog releases a sign-in that is still waiting.
  const release = () => {
    const pending = live.current;
    live.current = null;
    if (pending && new Date(pending.expiresAt).getTime() > Date.now())
      void request(`/api/oauth/sessions/${pending.id}`, "DELETE").catch(
        () => {},
      );
  };
  useEffect(() => release, []);

  useEffect(() => {
    if (!provider) return;
    let stopped = false;
    setSession(null);
    setPhase("starting");
    setError("");
    setCallback("");
    setCallbackError("");
    setSubmitted(false);
    request<Connection>(
      `/api/oauth/${provider}`,
      "POST",
      target ? { accountId: target } : provider === "codex" ? {} : undefined,
    )
      .then((next) => {
        if (stopped) {
          void request(`/api/oauth/sessions/${next.id}`, "DELETE").catch(
            () => {},
          );
          return;
        }
        live.current = next;
        setSession(next);
        setPhase("pending");
      })
      .catch((err) => {
        if (stopped) return;
        setPhase("error");
        setError(message(err, "Could not start sign-in."));
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
          live.current = null;
          setPhase("error");
          setError(result.error || "Provider sign-in failed.");
          return;
        }
        setError("");
        timer = setTimeout(poll, 2500);
      } catch (err) {
        if (!stopped) {
          setError(message(err, "Could not check sign-in."));
          timer = setTimeout(poll, 5000);
        }
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
      await request(`/api/oauth/sessions/${ready.id}/callback`, "POST", {
        redirectUrl: callback.trim(),
      });
      setCallback("");
      setSubmitted(true);
    } catch (err) {
      setCallbackError(
        message(err, "Could not finish sign-in. Check the URL and try again."),
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
      style={{ "--provider": providerColor(name) } as CSSProperties}
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
            Choose where to sign in. vrouter keeps the resulting tokens on the
            server. They never pass through this browser.
          </p>
          <div className="provider-choice">
            {connectable.map((p) => (
              <button
                key={p}
                style={{ "--provider": providerColor(p) } as CSSProperties}
                onClick={() => choose(p.toLowerCase())}
              >
                <ProviderBrand provider={p} />
                <span>
                  <strong>Sign in with {p}</strong>
                  {p === "Codex"
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
            <Check size={16} /> Connection saved to the {name} pool. vrouter has
            not sent a test request, so confirm with a client call.
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
                The official sign-in page opens in a new tab. Use the account
                you want to add.
              </p>
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
              <p>
                Approve access in the sign-in tab. This dialog updates
                automatically when vrouter receives the result.
              </p>
              <p
                className={ready ? "connect-wait" : "connect-wait unavailable"}
                role={ready ? "status" : undefined}
                inert={!ready}
              >
                <RefreshCw size={13} className="spinning" />
                {submitted
                  ? "URL accepted. Finishing sign-in."
                  : "Waiting for sign-in."}{" "}
                Link expires in {countdown}.
              </p>
            </li>
          </ol>
          {error && (
            <div className="notice error" role="alert">
              {error}
            </div>
          )}
          <section
            className={ready ? "connect-manual" : "connect-manual unavailable"}
            inert={!ready}
            aria-labelledby="callback-label"
          >
            <label id="callback-label" htmlFor="callback-url">
              Paste the return URL
            </label>
            <p id="callback-help">
              If sign-in doesn't finish here, copy the full URL from the sign-in
              tab after approving access and paste it below. This works even if
              that page can't load.
            </p>
            <form
              onSubmit={(e) => {
                e.preventDefault();
                void submit();
              }}
            >
              <input
                id="callback-url"
                type="url"
                value={callback}
                onChange={(e) => {
                  setCallback(e.target.value);
                  setCallbackError("");
                }}
                placeholder={`${ready?.redirectUri || "http://localhost/…"}?code=…&state=…`}
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
                  ? "URL accepted"
                  : submitting
                    ? "Connecting…"
                    : "Finish sign-in"}
              </button>
            </form>
            {callbackError && (
              <p id="callback-error" className="connect-url-error" role="alert">
                {callbackError}
              </p>
            )}
          </section>
        </>
      )}
    </dialog>
  );
}
