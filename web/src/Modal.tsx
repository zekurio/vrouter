import { useEffect, useRef, type ReactNode } from "react";
import { isDialogBackdropClick } from "./dialog";

// A native modal dialog that is open for as long as it is mounted.
export function Modal({
  className = "",
  labelledBy,
  onClose,
  locked = false,
  children,
}: {
  className?: string;
  labelledBy: string;
  onClose: () => void;
  // Ignore Escape and backdrop clicks, for work in flight or content that
  // cannot be shown again.
  locked?: boolean;
  children: ReactNode;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => dialog.current?.showModal(), []);
  return (
    // oxlint-disable-next-line jsx-a11y/click-events-have-key-events, jsx-a11y/no-noninteractive-element-interactions -- backdrop click; Escape is handled by onCancel
    <dialog
      ref={dialog}
      className={className}
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
