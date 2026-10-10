import { useEffect, useRef, useState, type CSSProperties } from "react";
import { LogIn, Plus, Trash2 } from "lucide-react";
import { ConnectDialog, connectable } from "./ConnectDialog";
import { ProviderBrand, providerColor, providerLabel } from "./ProviderBrand";
import { Private, usePrivateLabel } from "./Privacy";
import { QuotaWindows, type QuotaWindow } from "./Quota";
import { isDialogBackdropClick } from "./dialog";
import { errorMessage, type APIRequest } from "./api";

export type Account = {
  id: string;
  name: string;
  provider: string;
  status: string;
  plan: string;
  // Copied from the weekly window, or the first one. Null without windows.
  remaining: number | null;
  window: string;
  email?: string;
  statusMessage?: string;
  createdAt?: string;
  reconnectable?: boolean;
  authMode?: string;
  windows?: QuotaWindow[];
  quotaUpdatedAt?: string;
  quotaError?: string;
  availableResets?: number;
};
type Props = {
  accounts: Account[];
  request: APIRequest;
  // null is closed, "" asks which provider, otherwise the provider to sign in to.
  connect: string | null;
  setConnect: (provider: string | null) => void;
  onChanged: () => void;
  notify: (message: string) => void;
};

export function accountLabel(account: Account) {
  return account.email || `${providerLabel(account.provider)} account`;
}

// Badge for an account that is not routing. "connected" only says credentials
// are stored, so it gets no badge of its own.
export function accountState(account: Account) {
  if (account.status === "connected") return "";
  return account.status === "disabled" ? "Disabled" : "Unavailable";
}

export function AccountsPage({
  accounts,
  request,
  connect,
  setConnect,
  onChanged,
  notify,
}: Props) {
  // Switch positions that were saved but are not yet in the refreshed account list.
  const [saved, setSaved] = useState<Record<string, boolean>>({});
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [removing, setRemoving] = useState<Account | null>(null);
  const [removeError, setRemoveError] = useState("");
  // Account being reconnected. Cleared whenever the connect dialog closes.
  const [reauth, setReauth] = useState<Account | null>(null);
  const dialog = useRef<HTMLDialogElement>(null);
  const label = usePrivateLabel();
  useEffect(() => setSaved({}), [accounts]);
  useEffect(() => {
    if (connect === null) setReauth(null);
  }, [connect]);
  useEffect(() => {
    if (removing) dialog.current?.showModal();
    else dialog.current?.close();
  }, [removing]);

  const providers = [
    ...new Set([...connectable, ...accounts.map((a) => a.provider)]),
  ];
  const enabled = (a: Account) => saved[a.id] ?? a.status !== "disabled";

  async function toggle(account: Account) {
    const next = !enabled(account);
    setBusy(account.id);
    setError("");
    try {
      await request("/api/accounts", "PATCH", {
        id: account.id,
        enabled: next,
      });
      setSaved((current) => ({ ...current, [account.id]: next }));
      notify(next ? "Account enabled" : "Account disabled");
      onChanged();
    } catch (err) {
      setError(errorMessage(err, "Could not update the account."));
    } finally {
      setBusy("");
    }
  }
  async function remove() {
    if (!removing) return;
    setBusy(removing.id);
    setRemoveError("");
    try {
      await request("/api/accounts", "DELETE", { id: removing.id });
      setRemoving(null);
      notify("Account removed");
      onChanged();
    } catch (err) {
      setRemoveError(errorMessage(err, "Could not remove the account."));
    } finally {
      setBusy("");
    }
  }
  const closeRemove = () => {
    if (busy) return;
    setRemoving(null);
    setRemoveError("");
  };

  return (
    <>
      {error && (
        <div className="notice error" role="alert">
          {error}
        </div>
      )}
      {providers.map((provider) => {
        const pool = accounts.filter((a) => a.provider === provider);
        const canConnect = connectable.includes(provider);
        return (
          <section
            className="account-pool"
            key={provider}
            style={{ "--provider": providerColor(provider) } as CSSProperties}
            aria-labelledby={`pool-${provider}`}
          >
            <div className="model-group-heading">
              <ProviderBrand provider={provider} />
              <h2 id={`pool-${provider}`}>{providerLabel(provider)}</h2>
              <span>
                {pool.length} {pool.length === 1 ? "account" : "accounts"}
              </span>
              <div className="pool-actions">
                {canConnect && (
                  <button
                    className="secondary"
                    onClick={() => {
                      setReauth(null);
                      setConnect(provider);
                    }}
                  >
                    <Plus size={14} /> Add account
                  </button>
                )}
              </div>
            </div>
            {pool.length === 0 ? (
              <p className="empty">No accounts yet</p>
            ) : (
              <ul>
                {pool.map((a, i) => {
                  const on = enabled(a);
                  const name = label(
                    accountLabel(a),
                    `${provider} account ${i + 1}`,
                  );
                  return (
                    <li
                      className={`account-row ${on ? "" : "is-off"}`}
                      key={a.id}
                    >
                      <button
                        className="switch"
                        role="switch"
                        aria-checked={on}
                        aria-label={`Route requests through ${name}`}
                        disabled={busy === a.id}
                        onClick={() => void toggle(a)}
                      />
                      <div className="account-identity">
                        <h3>
                          <Private peek>{accountLabel(a)}</Private>
                        </h3>
                        {a.availableResets !== undefined && (
                          <p>
                            {a.availableResets} usage{" "}
                            {a.availableResets === 1 ? "reset" : "resets"}{" "}
                            available
                          </p>
                        )}
                        {!on ? (
                          <p>Disabled. Requests skip this account.</p>
                        ) : accountState(a) ? (
                          <p className="account-warning">
                            <Private>
                              {a.statusMessage ||
                                "Unavailable. vrouter is not routing to it right now."}
                            </Private>
                          </p>
                        ) : null}
                      </div>
                      <span className="plan account-plan">{a.plan}</span>
                      <div className="account-windows">
                        {a.windows?.length ? (
                          <QuotaWindows windows={a.windows} />
                        ) : (
                          <p className={a.quotaError ? "quota-error" : ""}>
                            <Private>
                              {a.quotaError || "Allowance not reported"}
                            </Private>
                          </p>
                        )}
                        {!!a.windows?.length && a.quotaError && (
                          <p className="quota-error">
                            <Private>{a.quotaError}</Private>
                          </p>
                        )}
                      </div>
                      <div className="account-row-actions">
                        {a.reconnectable && (
                          <button
                            className="icon-button"
                            aria-label={`Reconnect ${name}`}
                            title={`Reconnect ${name}`}
                            disabled={busy === a.id}
                            onClick={() => {
                              setReauth(a);
                              setConnect(provider);
                            }}
                          >
                            <LogIn size={14} />
                          </button>
                        )}
                        <button
                          className="icon-button is-danger"
                          aria-label={`Remove ${name}`}
                          onClick={() => setRemoving(a)}
                        >
                          <Trash2 size={15} />
                        </button>
                      </div>
                    </li>
                  );
                })}
              </ul>
            )}
          </section>
        );
      })}
      <dialog
        ref={dialog}
        className="confirm-dialog"
        onCancel={(e) => {
          e.preventDefault();
          closeRemove();
        }}
        onClick={(e) => {
          if (isDialogBackdropClick(e)) closeRemove();
        }}
        aria-labelledby="remove-title"
      >
        {removing && (
          <>
            <h2 id="remove-title">
              Remove <Private>{accountLabel(removing)}</Private>?
            </h2>
            <p>
              vrouter deletes the stored {providerLabel(removing.provider)}{" "}
              sign-in and stops routing requests to it. To use the account
              again, sign in from Add account.
            </p>
            {removeError && (
              <div className="notice error" role="alert">
                {removeError}
              </div>
            )}
            <div className="dialog-actions">
              <button
                className="secondary"
                disabled={!!busy}
                onClick={closeRemove}
              >
                Keep account
              </button>
              <button
                className="primary danger"
                disabled={!!busy}
                onClick={() => void remove()}
              >
                {busy ? "Removing" : "Remove account"}
              </button>
            </div>
          </>
        )}
      </dialog>
      {connect !== null && (
        <ConnectDialog
          key={reauth?.id ?? "new"}
          provider={connect}
          accountId={reauth?.id}
          request={request}
          choose={(next) => {
            setReauth(null);
            setConnect(next);
          }}
          onClose={() => {
            setReauth(null);
            setConnect(null);
          }}
          onConnected={onChanged}
        />
      )}
    </>
  );
}
