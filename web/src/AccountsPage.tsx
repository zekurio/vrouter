import { useEffect, useState } from "react";
import { AccountPool, type RowActions } from "./AccountPool";
import { ConnectDialog, connectable, type Scope } from "./ConnectDialog";
import { providerLabel } from "./ProviderBrand";
import type { QuotaWindow } from "./Quota";
import { errorMessage, type APIRequest } from "./api";
import { RemoveAccountDialog } from "./RemoveAccountDialog";
import type { AccountReset } from "./ResetDialog";
import { RoutingPanel } from "./RoutingPanel";

export type Account = {
  id: string;
  // Shared accounts serve everyone; personal ones only their owner.
  scope: Scope;
  // Set when this user may reconnect, disable or remove the account.
  manageable: boolean;
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
  // Admins manage shared accounts and the routing order.
  admin: boolean;
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
  admin,
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
  // Who a new account belongs to. Members only add personal accounts.
  const [scope, setScope] = useState<Scope>("personal");
  useEffect(() => setSaved({}), [accounts]);
  useEffect(() => {
    if (connect === null) {
      setReauth(null);
      setScope("personal");
    }
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
      <RoutingPanel admin={admin} request={request} notify={notify} />
      {scopes.map((section) => (
        <ScopeSection
          key={section.scope}
          section={section}
          // Members cannot add shared accounts.
          locked={section.scope === "global" && !admin}
          providers={providers}
          accounts={accounts}
          actions={actions}
          onAdd={(provider) => {
            setReauth(null);
            setScope(section.scope);
            setConnect(provider);
          }}
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
          scope={reauth?.scope ?? scope}
          setScope={admin ? setScope : undefined}
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

// The accounts of one scope, a pool per provider.
function ScopeSection({
  section,
  locked,
  providers,
  accounts,
  actions,
  onAdd,
}: {
  section: (typeof scopes)[number];
  // Admins manage this scope and the user is not one.
  locked: boolean;
  providers: string[];
  accounts: Account[];
  actions: RowActions;
  onAdd: (provider: string) => void;
}) {
  return (
    <section className="mt-7.5" aria-labelledby={`section-${section.scope}`}>
      <SectionHeading
        id={`section-${section.scope}`}
        title={section.title}
        lead={locked ? `${section.lead} Admins manage them.` : section.lead}
      />
      {providers.map((provider) => (
        <AccountPool
          key={provider}
          provider={provider}
          scope={section.scope}
          pool={accounts.filter(
            (a) => a.provider === provider && a.scope === section.scope,
          )}
          onAdd={
            locked || !connectable.includes(provider)
              ? undefined
              : () => onAdd(provider)
          }
          actions={actions}
        />
      ))}
    </section>
  );
}

function SectionHeading({
  id,
  title,
  lead,
}: {
  id: string;
  title: string;
  lead: string;
}) {
  return (
    <div className="mb-3.5 flex flex-wrap items-baseline gap-x-3.5 gap-y-1">
      <h2 id={id} className="text-[18px] font-semibold tracking-[-0.3px]">
        {title}
      </h2>
      <p className="text-muted">{lead}</p>
    </div>
  );
}

const scopes: { scope: Scope; title: string; lead: string }[] = [
  {
    scope: "personal",
    title: "Your accounts",
    lead: "Only your API keys use these. Nobody else sees them, admins included.",
  },
  {
    scope: "global",
    title: "Shared accounts",
    lead: "Every user's API keys can use these.",
  },
];
