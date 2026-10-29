import { useEffect, useState } from "react";
import { LogIn, Plus, Trash2 } from "lucide-react";
import { ConnectDialog, connectable, type Scope } from "./ConnectDialog";
import { ProviderBrand, providerLabel, providerStyle } from "./ProviderBrand";
import { Private, usePrivateLabel } from "./Privacy";
import { AccountWindows, type QuotaWindow } from "./Quota";
import { errorMessage, type APIRequest } from "./api";
import { RemoveAccountDialog } from "./RemoveAccountDialog";
import { RoutingPanel } from "./RoutingPanel";
import {
  button,
  cx,
  empty,
  groupCount,
  groupHeading,
  groupTitle,
  iconButton,
  notice,
  panelFrame,
  toggle as switchClass,
} from "./ui";

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
  availableResets?: number;
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
  const [error, setError] = useState("");
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
        <div className={cx(notice({ error: true }), "my-3.75")} role="alert">
          {error}
        </div>
      )}
      <RoutingPanel admin={admin} request={request} notify={notify} />
      {scopes.map((section) => (
        <section
          key={section.scope}
          className="mt-7.5"
          aria-labelledby={`section-${section.scope}`}
        >
          <SectionHeading
            id={`section-${section.scope}`}
            title={section.title}
            lead={
              section.scope === "global" && !admin
                ? `${section.lead} Admins manage them.`
                : section.lead
            }
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
                section.scope === "global" && !admin
                  ? undefined
                  : () => {
                      setReauth(null);
                      setScope(section.scope);
                      setConnect(provider);
                    }
              }
              actions={actions}
            />
          ))}
        </section>
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

function AccountPool({
  provider,
  scope,
  pool,
  onAdd,
  actions,
}: {
  provider: string;
  scope: Scope;
  pool: Account[];
  // Unset when the user cannot add accounts to this pool.
  onAdd: (() => void) | undefined;
  actions: RowActions;
}) {
  const canConnect = connectable.includes(provider) && onAdd !== undefined;
  const id = `pool-${scope}-${provider}`;
  // Pools after the first in a section stand apart from the one above.
  return (
    <section
      className={cx(
        panelFrame,
        "px-5.5 pt-5 pb-1.5 not-first-of-type:mt-3 max-sm:p-4.5",
      )}
      style={providerStyle(provider)}
      aria-labelledby={id}
    >
      <div
        className={cx(groupHeading, "flex-wrap border-b border-border pb-3.5")}
      >
        <ProviderBrand provider={provider} />
        <h3 id={id} className={groupTitle}>
          {providerLabel(provider)}
        </h3>
        <span className={cx(groupCount, "whitespace-nowrap")}>
          {pool.length} {pool.length === 1 ? "account" : "accounts"}
        </span>
        <div className="ml-auto flex gap-2">
          {canConnect && (
            <button className={button("secondary")} onClick={onAdd}>
              <Plus size={14} /> Add account
            </button>
          )}
        </div>
      </div>
      {pool.length === 0 ? (
        <p className={cx(empty, "mt-3 mb-2")}>
          {scope === "personal" ? "No personal accounts" : "No shared accounts"}
        </p>
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
    // Switch, identity, plan, usage, actions. Identity and plan have fixed
    // widths so the usage windows start at the same x on every row and
    // provider. Below 800px the fixed columns would squeeze the usage
    // windows, so the row stacks: name and actions on top, plan, then usage.
    <li className="grid min-h-16.5 grid-cols-[30px_minmax(0,208px)_88px_minmax(0,1fr)_70px] items-center gap-x-5 border-b border-border py-4 last:border-b-0 max-[1120px]:grid-cols-[30px_minmax(0,143px)_72px_minmax(0,1fr)_64px] max-[1120px]:gap-x-3.5 max-lg:grid-cols-[30px_minmax(0,1fr)_70px] max-lg:items-start max-lg:gap-y-3.5">
      <button
        className={cx(switchClass, "max-lg:mt-px")}
        role="switch"
        aria-checked={on}
        aria-label={`Route requests through ${name}`}
        title={a.manageable ? undefined : "Admins manage shared accounts"}
        disabled={busy === a.id || !a.manageable}
        onClick={() => actions.onToggle(a)}
      />
      {/* The name plus one secondary line, matching the quota block, centred
          as a block so a name-only account sits on the row's midline. */}
      <div className="flex min-h-11.5 flex-col justify-center max-lg:min-h-0">
        <h3 className={cx(accountName, !on && "opacity-50")}>
          <Private peek>{accountLabel(a)}</Private>
        </h3>
        {a.availableResets !== undefined && (
          <p className={cx(rowNote, "text-muted")}>
            {a.availableResets} usage{" "}
            {a.availableResets === 1 ? "reset" : "resets"} available
          </p>
        )}
        {on ? (
          accountState(a) && (
            <p className={cx(rowNote, "text-amber")}>
              <Private>
                {statusMessage ||
                  "Unavailable. vrouter is not routing to it right now."}
              </Private>
            </p>
          )
        ) : (
          <p className={cx(rowNote, "text-muted")}>
            Disabled. Requests skip this account.
          </p>
        )}
      </div>
      <span className={cx(plan, !on && "opacity-50")}>{a.plan}</span>
      <AccountWindows
        windows={a.windows ?? []}
        quotaError={a.quotaError ?? ""}
        off={!on}
      />
      <div className="flex justify-end gap-1.5 max-lg:col-start-3 max-lg:row-start-1 max-lg:-mt-1.75">
        {a.reconnectable === true && (
          <button
            className={iconButton()}
            aria-label={`Reconnect ${name}`}
            title={`Reconnect ${name}`}
            disabled={busy === a.id}
            onClick={() => actions.onReconnect(a)}
          >
            <LogIn size={14} />
          </button>
        )}
        {a.manageable && (
          <button
            className={iconButton("md", { danger: true })}
            aria-label={`Remove ${name}`}
            onClick={() => actions.onRemove(a)}
          >
            <Trash2 size={15} />
          </button>
        )}
      </div>
    </li>
  );
}

const accountName =
  "flex flex-wrap items-baseline gap-x-2.5 gap-y-0.5 text-[14px] leading-5 font-[550] wrap-anywhere";
// The secondary lines under an account's name.
const rowNote = "mt-1 text-[12px] leading-normal wrap-anywhere";
// An empty plan keeps rows lined up in the wide layout. The stacked layout
// drops it, so no blank line is left behind.
const plan =
  "inline-block text-[13px] leading-[1.4] font-medium text-provider wrap-anywhere max-lg:col-[2/4] max-lg:-mt-2 max-lg:empty:hidden";
