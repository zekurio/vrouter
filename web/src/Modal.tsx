import { useEffect, useRef, type ReactNode } from "react";
import { isDialogBackdropClick } from "./dialog";
import { dialog as dialogClass } from "./ui";

// A native modal dialog that is open for as long as it is mounted.
export function Modal({
  width = "md",
  labelledBy,
  onClose,
  locked = false,
  children,
}: {
  width?: "md" | "sm" | "xs";
  labelledBy: string;
  onClose: () => void;
  // Ignore Escape and backdrop clicks, for work in flight or content that
  // cannot be shown again.
  locked?: boolean;
  children: ReactNode;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const opener = document.activeElement;
    dialog.current?.showModal();
    // Unmounting skips the browser's own focus restore, so return focus to
    // whatever opened the dialog while it is still on the page.
    return () => {
      if (opener instanceof HTMLElement && opener.isConnected) opener.focus();
    };
  }, []);
  return (
    // oxlint-disable-next-line jsx-a11y/click-events-have-key-events, jsx-a11y/no-noninteractive-element-interactions -- backdrop click; Escape is handled by onCancel
    <dialog
      ref={dialog}
      className={dialogClass(width)}
      aria-labelledby={labelledBy}
      onCancel={(e) => {
        e.preventDefault();
        if (!locked) onClose();
      }}
      onClick={(e) => {
        if (!locked && isDialogBackdropClick(e)) onClose();
      }}
    >
      {children}
    </dialog>
  );
}
