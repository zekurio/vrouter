import { useEffect, useRef, useState } from "react";
import { X } from "lucide-react";
import { errorMessage, isStale, statusOf, type APIRequest } from "./api";
import { ProviderBrand, providerLabel, providerStyle } from "./ProviderBrand";
import { ConnectDone, ConnectFailed, ConnectSteps } from "./ConnectSteps";
import {
  cx,
  dialog as dialogClass,
  dialogHeading,
  dialogLead,
  dialogTitle as titleClass,
  iconButton,
  tab,
  tabs,
} from "./ui";

// Providers vrouter can sign in to. Codex is the pool for OpenAI sign-in.
export const connectable = ["codex", "claude"];
// Shared accounts serve everyone; personal ones only their owner.
export type Scope = "global" | "personal";

export type Connection = {
  id: string;
  provider: string;
  url: string;
  expiresAt: string;
  redirectUri?: string;
  flow: "loopback" | "code" | "device";
  userCode?: string;
};
type Phase = "starting" | "pending" | "connected" | "error";
type Props = {
  // "" shows the provider choice; a provider ID starts sign-in right away.
  provider: string;
  // Set when renewing a saved account instead of registering a new one.
  accountId?: string | undefined;
  // Who a new account belongs to.
  scope: Scope;
  // Lets an admin choose the scope along with the provider.
  setScope: ((scope: Scope) => void) | undefined;
  request: APIRequest;
  choose: (provider: string) => void;
  onClose: () => void;
  onConnected: () => void;
};

// Starts a sign-in on the server and polls it until it finishes. Leaving the
// dialog or starting over releases the session.
function useSignIn({
  provider,
  target,
  scope,
  attempt,
  request,
  onConnected,
}: {
  provider: string;
  target: string;
  scope: Scope;
  // Bumped to start over with the same provider and target.
  attempt: number;
  request: APIRequest;
  onConnected: () => void;
}) {
  const [session, setSession] = useState<Connection | null>(null);
  const [phase, setPhase] = useState<Phase>("starting");
  const [error, setError] = useState("");
  const [now, setNow] = useState(Date.now());
  const onConnectedRef = useRef(onConnected);
  onConnectedRef.current = onConnected;
  const live = useRef<Connection | null>(null);

  useEffect(() => {
    if (!provider) return undefined;
    let stopped = false;
    // Frees a sign-in the server still holds. DELETE goes out even after the
    // client has closed.
    const cancel = (sessionId: string) =>
      void request(`/api/oauth/sessions/${sessionId}`, "DELETE").catch(
        () => {},
      );
    const start = async () => {
      try {
        const next = await request<Connection>(
          `/api/oauth/${provider}`,
          "POST",
          target ? { accountId: target } : { scope },
        );
        if (stopped) {
          cancel(next.id);
          return;
        }
        live.current = next;
        setNow(Date.now());
        setSession(next);
        setPhase("pending");
      } catch (err) {
        if (stopped) return;
        setPhase("error");
        setError(errorMessage(err, "Could not start sign-in."));
      }
    };
    void start();
    // Leaving the dialog or starting over releases the session.
    return () => {
      stopped = true;
      const pending = live.current;
      live.current = null;
      if (pending && new Date(pending.expiresAt).getTime() > Date.now())
        cancel(pending.id);
    };
  }, [provider, attempt, target, scope, request]);

  useEffect(() => {
    if (!session || phase !== "pending") return undefined;
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
          setError((result.error ?? "") || "Provider sign-in failed.");
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

  // Drops the session and its error, so a new attempt starts from "starting".
  const reset = () => {
    setSession(null);
    setPhase("starting");
    setError("");
  };
  return { session, phase, error, now, reset };
}

function dialogTitle(phase: Phase, name: string, target: string, scope: Scope) {
  if (!name) return "Add an account";
  if (phase === "connected")
    return target ? `${name} account reconnected` : `${name} sign-in saved`;
  if (target) return `Reconnect ${name} account`;
  return scope === "global"
    ? `Add a shared ${name} account`
    : `Add a ${name} account`;
}

export function ConnectDialog({
  provider,
  accountId,
  scope,
  setScope,
  request,
  choose,
  onClose,
  onConnected,
}: Props) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [callback, setCallback] = useState("");
  const [callbackError, setCallbackError] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [submitted, setSubmitted] = useState(false);
  const [attempt, setAttempt] = useState(0);
  // Reconnect applies to the first attempt and retries. Add another starts fresh.
  const [target, setTarget] = useState(accountId ?? "");
  useEffect(() => {
    const opener = document.activeElement;
    dialog.current?.showModal();
    // Unmounting skips the browser's own focus restore.
    return () => {
      if (opener instanceof HTMLElement && opener.isConnected) opener.focus();
    };
  }, []);
  const { session, phase, error, now, reset } = useSignIn({
    provider,
    target,
    scope,
    attempt,
    request,
    onConnected,
  });
  const id = connectable.includes(provider) ? provider : "";
  const name = id && providerLabel(id);
  // Only a live session may be linked. Controls without one keep their space.
  const ready = phase === "pending" ? session : null;
  const manualCode = session?.flow === "code";

  // Clears the old session in the same render so its link cannot be clicked.
  function restart(next: string) {
    reset();
    setCallback("");
    setCallbackError("");
    setSubmitted(false);
    setTarget(next);
    setAttempt((n) => n + 1);
  }

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

  return (
    <dialog
      ref={dialog}
      className={dialogClass("sm")}
      style={providerStyle(id)}
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
      aria-labelledby="connect-title"
    >
      <div className={dialogHeading}>
        <h2 id="connect-title" className={titleClass}>
          {dialogTitle(phase, name, target, scope)}
        </h2>
        {phase !== "connected" && phase !== "error" && (
          <button
            className={iconButton()}
            aria-label={name ? "Cancel sign-in" : "Close dialog"}
            onClick={onClose}
          >
            <X size={18} />
          </button>
        )}
      </div>
      {name === "" ? (
        <ProviderChoice choose={choose} scope={scope} setScope={setScope} />
      ) : phase === "connected" ? (
        <ConnectDone
          name={name}
          scope={scope}
          onAnother={() => restart("")}
          onClose={onClose}
        />
      ) : phase === "error" ? (
        <ConnectFailed
          error={error}
          onClose={onClose}
          onRetry={() => restart(target)}
        />
      ) : (
        <ConnectSteps
          name={name}
          session={session}
          ready={ready}
          now={now}
          error={error}
          callback={{
            value: callback,
            error: callbackError,
            submitting,
            submitted,
            onChange: (value) => {
              setCallback(value);
              setCallbackError("");
            },
            onSubmit: () => void submit(),
          }}
        />
      )}
    </dialog>
  );
}

const scopeChoices: { value: Scope; label: string }[] = [
  { value: "personal", label: "Personal" },
  { value: "global", label: "Shared" },
];

function ProviderChoice({
  choose,
  scope,
  setScope,
}: {
  choose: (provider: string) => void;
  scope: Scope;
  setScope: ((scope: Scope) => void) | undefined;
}) {
  return (
    <>
      <p className={dialogLead}>
        Tokens stay on the server and never pass through this browser.{" "}
        {scope === "global"
          ? "Every user's API keys can use a shared account."
          : "Only your API keys use a personal account."}
      </p>
      {setScope && (
        <div
          className={cx(tabs(true), "mt-4.5 w-fit")}
          role="group"
          aria-label="Owner"
        >
          {scopeChoices.map((c) => (
            <button
              key={c.value}
              aria-pressed={scope === c.value}
              className={tab(scope === c.value)}
              onClick={() => setScope(c.value)}
            >
              {c.label}
            </button>
          ))}
        </div>
      )}
      <div className="mt-5 grid grid-cols-2 gap-2.5 max-sm:grid-cols-1">
        {connectable.map((p) => (
          <button
            key={p}
            className="flex items-center gap-3.25 rounded-xl border border-line bg-well p-4 text-left hover:border-provider hover:bg-chip"
            style={providerStyle(p)}
            onClick={() => choose(p)}
          >
            <ProviderBrand provider={p} />
            <span className="flex flex-col gap-0.75 text-[11px] text-muted">
              <strong className="text-[13px] font-[550] text-text">
                Sign in with {providerLabel(p)}
              </strong>
              {p === "codex" ? "ChatGPT subscription" : "Claude subscription"}
            </span>
          </button>
        ))}
      </div>
    </>
  );
}
