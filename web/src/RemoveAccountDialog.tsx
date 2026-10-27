import { useEffect, useRef } from "react";
import { accountLabel, type Account } from "./AccountsPage";
import { isDialogBackdropClick } from "./dialog";
import { Private } from "./Privacy";
import { providerLabel } from "./ProviderBrand";

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
            sign-in and stops routing requests to it
            {removing.scope === "global" ? " for everyone" : ""}. To use the
            account again, sign in from Add account.
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
