import { useState } from "react";
import { LogIn, Plus, Trash2 } from "lucide-react";
import { accountLabel, accountState, type Account } from "./AccountsPage";
import { errorMessage, type APIRequest } from "./api";
import { ProviderBrand, providerLabel, providerStyle } from "./ProviderBrand";
import { Private, usePrivateLabel } from "./Privacy";
import { AccountWindows, QuotaIssue } from "./Quota";
import { ResetDialog, resetSummary } from "./ResetDialog";
import { useToast } from "./Toaster";
import {
  button,
  cx,
  groupCount,
  groupHeading,
  groupTitle,
  iconButton,
  panelFrame,
  toggle as switchClass,
} from "./ui";

// What a row can do to its account, shared by every pool.
export type RowActions = {
  busy: string;
  enabled: (account: Account) => boolean;
  // Rejects when the switch could not be saved.
  onToggle: (account: Account) => Promise<void>;
  onReconnect: (account: Account) => void;
  onRemove: (account: Account) => void;
  request: APIRequest;
  // Called after the provider confirmed a usage reset.
  onResetUsed: () => void;
};

export function AccountPool({
  provider,
  pool,
  onAdd,
  actions,
}: {
  provider: string;
  pool: Account[];
  // Unset for a provider vrouter cannot sign in to.
  onAdd: (() => void) | undefined;
  actions: RowActions;
}) {
  const id = `pool-${provider}`;
  // Pools after the first stand apart from the one above.
  return (
    <section
      className={cx(
        panelFrame,
        "px-5.5 pt-5 not-first-of-type:mt-3 max-sm:px-4.5 max-sm:pt-4.5",
        pool.length > 0 ? "pb-1.5 max-sm:pb-4.5" : "pb-5 max-sm:pb-4.5",
      )}
      style={providerStyle(provider)}
      aria-labelledby={id}
    >
      {/* A pool without accounts is its heading alone. */}
      <div
        className={cx(
          groupHeading,
          "flex-wrap",
          pool.length > 0 && "border-b border-border pb-3.5",
        )}
      >
        <ProviderBrand provider={provider} />
        <h3 id={id} className={groupTitle}>
          {providerLabel(provider)}
        </h3>
        <span className={cx(groupCount, "whitespace-nowrap")}>
          {pool.length} {pool.length === 1 ? "account" : "accounts"}
        </span>
        <div className="ml-auto flex gap-2">
          {onAdd && (
            <button className={button("secondary")} onClick={onAdd}>
              <Plus size={14} /> Add account
            </button>
          )}
        </div>
      </div>
      {pool.length > 0 && (
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
  const toast = useToast();
  // A switch that could not be saved stays where it was. The toast says
  // which account, since the row may have scrolled away.
  const flip = () => {
    actions.onToggle(a).catch((err: unknown) =>
      toast(
        `${name} is still ${on ? "enabled" : "disabled"}. ${errorMessage(err, "Could not update the account.")}`,
        {
          failed: true,
          id: `toggle-${a.id}`,
          action: { label: "Retry", run: flip },
        },
      ),
    );
  };
  return (
    // Switch, identity, plan, usage, actions. Identity and plan have fixed
    // widths so the usage windows start at the same x on every row and
    // provider. Below 800px the fixed columns would squeeze the usage
    // windows, so the row stacks: name and actions on top, plan, then usage.
    <li className="grid min-h-16.5 grid-cols-[30px_minmax(0,208px)_88px_minmax(0,1fr)_70px] items-center gap-x-5 border-b border-border py-4 last:border-b-0 max-[1120px]:grid-cols-[30px_minmax(0,143px)_72px_minmax(0,1fr)_64px] max-[1120px]:gap-x-3.5 max-md:grid-cols-[30px_minmax(0,1fr)_auto] max-md:items-start max-md:gap-y-3.5">
      <button
        className={cx(switchClass, "max-md:mt-px")}
        role="switch"
        aria-checked={on}
        aria-label={`Route requests through ${name}`}
        disabled={busy === a.id}
        onClick={flip}
      />
      {/* The name plus one secondary line, matching the quota block, centred
          as a block so a name-only account sits on the row's midline. */}
      <div className="flex min-h-11.5 flex-col justify-center max-md:min-h-0 max-sm:col-[2/4]">
        <div className="relative flex items-center gap-2">
          <h3 className={cx(accountName, "min-w-0", !on && "opacity-50")}>
            <Private peek>{accountLabel(a)}</Private>
          </h3>
          <QuotaIssue account={a} />
        </div>
        <AccountResets account={a} actions={actions} />
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
        failed={(a.quotaError ?? "") !== ""}
        off={!on}
      />
      <div className="flex justify-end gap-1.5 max-md:col-start-3 max-md:row-start-1 max-md:-mt-1.75 max-sm:row-start-2 max-sm:-my-2.5 max-sm:-mr-2.5">
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
        <button
          className={iconButton("md", { danger: true })}
          aria-label={`Remove ${name}`}
          onClick={() => actions.onRemove(a)}
        >
          <Trash2 size={15} />
        </button>
      </div>
    </li>
  );
}

const accountName = "text-[14px] leading-5 font-[550] wrap-break-word";
// The secondary lines under an account's name.
const rowNote = "mt-1 text-[12px] leading-normal wrap-break-word";
// An empty plan keeps rows lined up in the wide layout. The stacked layout
// drops it, so no blank line is left behind.
const plan =
  "inline-block text-[13px] leading-[1.4] font-medium text-provider wrap-anywhere max-md:col-[2/4] max-md:-mt-2 max-md:empty:hidden max-sm:col-[2/3] max-sm:mt-0 max-sm:self-center";

// The resets an account holds, under its name. They open a dialog that uses
// one.
function AccountResets({
  account: a,
  actions,
}: {
  account: Account;
  actions: RowActions;
}) {
  const [open, setOpen] = useState(false);
  const resets = a.resets ?? [];
  if (resets.length === 0) return null;
  return (
    <>
      <button
        type="button"
        className={cx(
          rowNote,
          "text-left text-muted underline decoration-line underline-offset-3 hover:text-text",
        )}
        onClick={() => setOpen(true)}
      >
        {resetSummary(resets)}
      </button>
      {open && (
        <ResetDialog
          account={a}
          request={actions.request}
          onClose={() => setOpen(false)}
          onUsed={() => {
            setOpen(false);
            actions.onResetUsed();
          }}
        />
      )}
    </>
  );
}
