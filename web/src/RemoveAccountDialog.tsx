import { useEffect, useRef } from "react";
import { accountLabel, type Account } from "./AccountsPage";
import { isDialogBackdropClick } from "./dialog";
import { Private } from "./Privacy";
import { providerLabel } from "./ProviderBrand";
import {
  button,
  confirmTitle,
  cx,
  dialog as dialogClass,
  dialogActions,
  dialogLead,
  notice,
} from "./ui";

// Asks before an account's stored sign-in is deleted.
export function RemoveAccountDialog({
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
      className={dialogClass("xs")}
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
          <h2 id="remove-title" className={confirmTitle}>
            Remove <Private>{accountLabel(removing)}</Private>?
          </h2>
          <p className={dialogLead}>
            vrouter deletes the stored {providerLabel(removing.provider)}{" "}
            sign-in and stops routing requests to it
            {removing.scope === "global" ? " for everyone" : ""}. To use the
            account again, sign in from Add account.
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
              className={button("secondary")}
              disabled={!!busy}
              onClick={onClose}
            >
              Keep account
            </button>
            <button
              className={button("danger")}
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
