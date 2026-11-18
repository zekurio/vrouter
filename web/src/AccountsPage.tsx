import { useEffect, useState } from "react";
import { AccountPool, type RowActions } from "./AccountPool";
import { ConnectDialog, connectable } from "./ConnectDialog";
import { providerLabel } from "./ProviderBrand";
import type { QuotaWindow } from "./Quota";
import { errorMessage, type APIRequest } from "./api";
import { RemoveAccountDialog } from "./RemoveAccountDialog";
import type { AccountReset } from "./ResetDialog";

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
  // When vrouter next reads usage after quotaError.
  quotaRetryAt?: string;
  // The usage resets the account holds, usable or not.
  resets?: AccountReset[];
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

  // Rejects when the switch could not be saved. The row says so.
  async function toggle(account: Account) {
    const next = !enabled(account);
    setBusy(account.id);
    try {
      await request("/api/accounts", "PATCH", {
        id: account.id,
        enabled: next,
      });
      setSaved((current) => ({ ...current, [account.id]: next }));
      notify(next ? "Account enabled" : "Account disabled");
      onChanged();
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
    onToggle: toggle,
    onReconnect: (a) => {
      setReauth(a);
      setConnect(a.provider);
    },
    onRemove: setRemoving,
    request,
    onResetUsed: () => {
      notify("Reset used");
      onChanged();
    },
  };

  return (
    <>
      {/* A pool per provider. */}
      <div>
        {providers.map((provider) => (
          <AccountPool
            key={provider}
            provider={provider}
            pool={accounts.filter((a) => a.provider === provider)}
            onAdd={
              connectable.includes(provider)
                ? () => {
                    setReauth(null);
                    setConnect(provider);
                  }
                : undefined
            }
            actions={actions}
          />
        ))}
      </div>
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
