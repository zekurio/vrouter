import { useEffect, useRef, useState } from "react";
import { LogIn, Plus, Trash2 } from "lucide-react";
import { ConnectDialog, connectable } from "./ConnectDialog";
import { ProviderBrand, providerLabel, providerStyle } from "./ProviderBrand";
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
  const email = account.email ?? "";
  return email || `${providerLabel(account.provider)} account`;
}

// Badge for an account that is not routing. "connected" only says credentials
// are stored, so it gets no badge of its own.
export function accountState(account: Account) {
  if (account.status === "connected") return "";
  return account.status === "disabled" ? "Disabled" : "Unavailable";
}

// What a row can do to its account, shared by every pool.
type RowActions = {
  busy: string;
  enabled: (account: Account) => boolean;
  onToggle: (account: Account) => void;
  onReconnect: (account: Account) => void;
  onRemove: (account: Account) => void;
};

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
  useEffect(() => setSaved({}), [accounts]);
  useEffect(() => {
    if (connect === null) setReauth(null);
  }, [connect]);

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
  const actions: RowActions = {
    busy,
    enabled,
    onToggle: (a) => void toggle(a),
    onReconnect: (a) => {
      setReauth(a);
      setConnect(a.provider);
    },
    onRemove: setRemoving,
  };

  return (
    <>
      {error && (
        <div className="notice error" role="alert">
          {error}
        </div>
      )}
      {providers.map((provider) => (
        <AccountPool
          key={provider}
          provider={provider}
          pool={accounts.filter((a) => a.provider === provider)}
          onAdd={() => {
            setReauth(null);
            setConnect(provider);
          }}
          actions={actions}
        />
      ))}
      <RemoveAccountDialog
        removing={removing}
        busy={busy}
        error={removeError}
        onClose={closeRemove}
        onRemove={() => void remove()}
      />
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

function AccountPool({
  provider,
  pool,
  onAdd,
  actions,
}: {
  provider: string;
  pool: Account[];
  onAdd: () => void;
  actions: RowActions;
}) {
  const canConnect = connectable.includes(provider);
  return (
    <section
      className="account-pool"
      style={providerStyle(provider)}
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
            <button className="secondary" onClick={onAdd}>
              <Plus size={14} /> Add account
            </button>
          )}
        </div>
      </div>
      {pool.length === 0 ? (
        <p className="empty">No accounts yet</p>
      ) : (
        <ul>
          {pool.map((a, i) => (
            <AccountRow
              key={a.id}
              account={a}
              fallback={`${provider} account ${i + 1}`}
              actions={actions}
            />
          ))}
        </ul>
      )}
    </section>
  );
}

function AccountRow({
  account: a,
  fallback,
  actions,
}: {
  account: Account;
  // Label used in place of one that holds a hidden address.
  fallback: string;
  actions: RowActions;
}) {
  const label = usePrivateLabel();
  const { busy } = actions;
  const on = actions.enabled(a);
  const statusMessage = a.statusMessage ?? "";
  const name = label(accountLabel(a), fallback);
  return (
    <li className={`account-row ${on ? "" : "is-off"}`}>
      <button
        className="switch"
        role="switch"
        aria-checked={on}
        aria-label={`Route requests through ${name}`}
        disabled={busy === a.id}
        onClick={() => actions.onToggle(a)}
      />
      <div className="account-identity">
        <h3>
          <Private peek>{accountLabel(a)}</Private>
        </h3>
        {a.availableResets !== undefined && (
          <p>
            {a.availableResets} usage{" "}
            {a.availableResets === 1 ? "reset" : "resets"} available
          </p>
        )}
        {on ? (
          accountState(a) && (
            <p className="account-warning">
              <Private>
                {statusMessage ||
                  "Unavailable. vrouter is not routing to it right now."}
              </Private>
            </p>
          )
        ) : (
          <p>Disabled. Requests skip this account.</p>
        )}
      </div>
      <span className="plan account-plan">{a.plan}</span>
      <AccountWindows account={a} />
      <div className="account-row-actions">
        {a.reconnectable === true && (
          <button
            className="icon-button"
            aria-label={`Reconnect ${name}`}
            title={`Reconnect ${name}`}
            disabled={busy === a.id}
            onClick={() => actions.onReconnect(a)}
          >
            <LogIn size={14} />
          </button>
        )}
        <button
          className="icon-button is-danger"
          aria-label={`Remove ${name}`}
          onClick={() => actions.onRemove(a)}
        >
          <Trash2 size={15} />
        </button>
      </div>
    </li>
  );
}

function AccountWindows({ account }: { account: Account }) {
  const windows = account.windows ?? [];
  const quotaError = account.quotaError ?? "";
  return (
    <div className="account-windows">
      {windows.length > 0 ? (
        <QuotaWindows windows={windows} />
      ) : (
        <p className={quotaError ? "quota-error" : ""}>
          <Private>{quotaError || "Allowance not reported"}</Private>
        </p>
      )}
      {windows.length > 0 && quotaError && (
        <p className="quota-error">
          <Private>{quotaError}</Private>
        </p>
      )}
    </div>
  );
}

function RemoveAccountDialog({
  removing,
  busy,
  error,
  onClose,
  onRemove,
}: {
  removing: Account | null;
  busy: string;
  error: string;
  onClose: () => void;
  onRemove: () => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    if (removing) dialog.current?.showModal();
    else dialog.current?.close();
  }, [removing]);
  return (
    // oxlint-disable-next-line jsx-a11y/click-events-have-key-events, jsx-a11y/no-noninteractive-element-interactions -- backdrop click; Escape is handled by onCancel
    <dialog
      ref={dialog}
      className="confirm-dialog"
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
      onClick={(e) => {
        if (isDialogBackdropClick(e)) onClose();
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
            sign-in and stops routing requests to it. To use the account again,
            sign in from Add account.
          </p>
          {error && (
            <div className="notice error" role="alert">
              {error}
            </div>
          )}
          <div className="dialog-actions">
            <button className="secondary" disabled={!!busy} onClick={onClose}>
              Keep account
            </button>
            <button
              className="primary danger"
              disabled={!!busy}
              onClick={onRemove}
            >
              {busy ? "Removing" : "Remove account"}
            </button>
          </div>
        </>
      )}
    </dialog>
  );
}
