import { useEffect, useRef, useState } from "react";
import { accountLabel, type Account } from "./AccountsPage";
import { errorMessage, type APIRequest } from "./api";
import { Modal } from "./Modal";
import { Private } from "./Privacy";
import {
  button,
  confirmTitle,
  cx,
  dialogActions,
  dialogLead,
  notice,
} from "./ui";

// One kind of usage reset an account holds.
export type AccountReset = {
  // The Claude grant to redeem. Empty for Codex, which has one pool.
  id: string;
  kind: string;
  label?: string;
  left: number;
  expiresAt?: string;
  // The provider accepts the reset now. note says why not.
  usable: boolean;
  note?: string;
};

// Each kind's name at the start of a line and after a count, and what using
// one does.
type Kind = { name: string; phrase: string; effect: string };
const credit: Kind = {
  name: "Usage reset",
  phrase: "usage reset",
  effect: "Resets the account's usage limits.",
};
const kinds: Partial<Record<string, Kind>> = {
  session: {
    name: "5-hour reset",
    phrase: "5-hour reset",
    effect: "Refills the 5-hour limit.",
  },
  weekly: {
    name: "Weekly reset",
    phrase: "weekly reset",
    effect: "Refills the weekly limits.",
  },
  fable: {
    name: "Fable reset",
    phrase: "Fable reset",
    effect:
      "Gives back the weekly usage spent on Fable models and refills the 5-hour limit.",
  },
  full: {
    name: "Full reset",
    phrase: "full reset",
    effect: "Refills the 5-hour and weekly limits.",
  },
  credit,
};
const kindOf = (reset: AccountReset) => kinds[reset.kind] ?? credit;

// "2 Fable resets · 1 weekly reset", for the line under an account's name.
export function resetSummary(resets: AccountReset[]) {
  return resets
    .map((r) => `${r.left} ${kindOf(r).phrase}${r.left === 1 ? "" : "s"}`)
    .join(" · ");
}

// The server waits up to 40 seconds for the provider to answer a reset.
const resetTimeout = 45000;

// Lists an account's resets and spends one after a second confirmation. A
// reset cannot be undone, so nothing is sent on the first click.
export function ResetDialog({
  account,
  request,
  onClose,
  onUsed,
}: {
  account: Account;
  request: APIRequest;
  onClose: () => void;
  // Called after the provider confirmed a reset.
  onUsed: () => void;
}) {
  const [chosen, setChosen] = useState<AccountReset | null>(null);
  // The reset whose confirmation was turned down. Its button gets the focus
  // back.
  const [kept, setKept] = useState<AccountReset | null>(null);
  const keep = useRef<HTMLButtonElement>(null);
  // The confirmation replaces the list, and with it the button that had the
  // focus. Start on the choice that spends nothing.
  useEffect(() => {
    if (chosen) keep.current?.focus();
  }, [chosen]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const resets = account.resets ?? [];

  // Set at once, so two quick clicks cannot send two resets.
  const sending = useRef(false);
  async function use(reset: AccountReset) {
    if (sending.current) return;
    sending.current = true;
    setBusy(true);
    setError("");
    try {
      await request(
        `/api/accounts/${encodeURIComponent(account.id)}/resets`,
        "POST",
        { grantId: reset.id },
        resetTimeout,
      );
      onUsed();
    } catch (err) {
      setError(errorMessage(err, "Could not use the reset."));
    } finally {
      sending.current = false;
      setBusy(false);
    }
  }

  return (
    <Modal width="sm" labelledBy="reset-title" onClose={onClose} locked={busy}>
      {chosen ? (
        <>
          <h2 id="reset-title" className={confirmTitle}>
            Use a {kindOf(chosen).phrase} on{" "}
            <Private>{accountLabel(account)}</Private>?
          </h2>
          <p className={dialogLead}>
            {kindOf(chosen).effect} This cannot be undone.
            {chosen.left > 1 ? ` ${chosen.left - 1} will be left.` : ""}
          </p>
          {error && (
            <div
              className={cx(notice({ error: true, inset: true }), "mt-4.5")}
              role="alert"
            >
              {error}
            </div>
          )}
          <div className={cx(dialogActions, "mt-6")}>
            <button
              ref={keep}
              className={button("secondary")}
              disabled={busy}
              onClick={() => {
                setKept(chosen);
                setChosen(null);
                setError("");
              }}
            >
              Keep it
            </button>
            {/* Not disabled while it works: a disabled button drops the
                keyboard focus, and a refusal would leave it nowhere. */}
            <button
              className={cx(
                button("primary"),
                "aria-disabled:cursor-default aria-disabled:opacity-50",
              )}
              aria-disabled={busy}
              onClick={() => void use(chosen)}
            >
              {busy ? "Using reset" : "Use reset"}
            </button>
          </div>
        </>
      ) : (
        <>
          <h2 id="reset-title" className={confirmTitle}>
            Usage resets for <Private>{accountLabel(account)}</Private>
          </h2>
          <p className={dialogLead}>
            A reset gives usage back right away. The limits still reset on their
            usual schedule.
          </p>
          <ul className="mt-4.5">
            {resets.map((r) => (
              <ResetRow
                key={r.id || r.kind}
                reset={r}
                focus={
                  kept !== null && kept.id === r.id && kept.kind === r.kind
                }
                onUse={setChosen}
              />
            ))}
          </ul>
          <div className={cx(dialogActions, "mt-6")}>
            <button className={button("secondary")} onClick={onClose}>
              Close
            </button>
          </div>
        </>
      )}
    </Modal>
  );
}

function ResetRow({
  reset,
  focus,
  onUse,
}: {
  reset: AccountReset;
  // Take the focus when the list comes back from this reset's confirmation.
  focus: boolean;
  onUse: (reset: AccountReset) => void;
}) {
  const use = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (focus) use.current?.focus();
  }, [focus]);
  const kind = kindOf(reset);
  const expires = reset.expiresAt ?? "";
  const note = reset.note ?? "";
  return (
    <li className="flex items-center gap-4 border-t border-border py-3.5 first:border-t-0">
      <div className="min-w-0 flex-1">
        <p className="text-[14px] font-[550]">
          {kind.name}
          <span className="ml-2 text-[13px] font-normal text-muted tabular-nums">
            {reset.left} left
          </span>
        </p>
        <p className="mt-1 text-[13px] leading-[1.5] text-subtle">
          {kind.effect}
        </p>
        {expires !== "" && (
          <p className="mt-1 text-[12px] text-muted">
            Use by{" "}
            <time dateTime={expires}>
              {new Date(expires).toLocaleString(undefined, {
                dateStyle: "medium",
                timeStyle: "short",
              })}
            </time>
          </p>
        )}
        {!reset.usable && (
          <p className="mt-1 text-[12px] leading-normal text-amber">
            {note || "Not usable right now."}
          </p>
        )}
      </div>
      <button
        ref={use}
        className={button("secondary")}
        disabled={!reset.usable}
        onClick={() => onUse(reset)}
      >
        Use
      </button>
    </li>
  );
}
