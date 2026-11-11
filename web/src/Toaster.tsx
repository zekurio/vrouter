import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { createPortal } from "react-dom";
import { Check, TriangleAlert, X } from "lucide-react";
import { cx } from "./ui";

type ToastOptions = {
  // The action did not work. Failures stay longer than confirmations.
  failed?: boolean;
  // A button beside the text, such as Retry. A toast with one stays until
  // the button or the close button is used.
  action?: { label: string; run: () => void };
  // A new toast replaces one with the same ID, so retrying the same thing
  // does not pile messages up. Defaults to the text.
  id?: string;
};
type Toast = ToastOptions & { key: number; id: string; text: string };
export type ShowToast = (text: string, options?: ToastOptions) => void;

// How many toasts show at once. An older one makes room for a newer one.
const visible = 3;

const ToastContext = createContext<ShowToast>(() => undefined);

// Shows a short message for something the user just did. Errors that stay
// true, or that belong to a field, a row or an open dialog, go there instead.
export const useToast = () => useContext(ToastContext);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const next = useRef(0);
  const show = useCallback<ShowToast>((text, options = {}) => {
    const id = options.id ?? text;
    const toast = { ...options, key: next.current++, id, text };
    setToasts((current) =>
      [...current.filter((t) => t.id !== id), toast].slice(-visible),
    );
  }, []);
  const dismiss = useCallback(
    (key: number) =>
      setToasts((current) => current.filter((t) => t.key !== key)),
    [],
  );
  return (
    <ToastContext value={show}>
      {children}
      <ToastStack toasts={toasts} dismiss={dismiss} />
    </ToastContext>
  );
}

// How long a toast stays. One with an action waits for the user.
function lifetime(toast: Toast) {
  if (toast.action) return 0;
  return toast.failed === true ? 8000 : 4000;
}

// The modal dialog on top, or null. Everything outside an open modal is
// inert: it takes no clicks and is not announced. Toasts therefore live
// inside the dialog while one is open.
function useTopModal() {
  const [modal, setModal] = useState<Element | null>(null);
  useEffect(() => {
    const find = () => {
      try {
        setModal([...document.querySelectorAll("dialog:modal")].at(-1) ?? null);
      } catch {
        // A browser without the selector keeps the toasts in the page.
      }
    };
    find();
    // Dialogs open, close and unmount without telling anyone.
    const observer = new MutationObserver(find);
    observer.observe(document.body, {
      subtree: true,
      childList: true,
      attributeFilter: ["open"],
    });
    return () => observer.disconnect();
  }, []);
  return modal;
}

function ToastStack({
  toasts,
  dismiss,
}: {
  toasts: Toast[];
  dismiss: (key: number) => void;
}) {
  const modal = useTopModal();
  const latest = toasts.at(-1);
  // One element holds the stack for good, and moves between the page and
  // the dialog. The toasts inside stay mounted through the move, so their
  // timers run on and a pointer resting on one still holds it.
  const [root] = useState(() => document.createElement("div"));
  useEffect(() => {
    (modal ?? document.body).append(root);
  }, [modal, root]);
  useEffect(() => () => root.remove(), [root]);
  return createPortal(
    <>
      {/* Screen readers announce text added to a live region that is already
          there, not a toast that mounts with a role. */}
      <div className="sr-only" aria-live="polite">
        {latest && latest.failed !== true ? latest.text : ""}
      </div>
      <div className="sr-only" aria-live="assertive">
        {latest?.failed === true ? latest.text : ""}
      </div>
      {/* Phones show toasts at the top, clear of the navigation bar, the
          Models save bar and the keyboard. */}
      <div
        role="region"
        aria-label="Notifications"
        className="pointer-events-none fixed inset-x-0 bottom-6.25 z-30 flex flex-col items-center gap-2 px-4 max-sm:top-[calc(12px+env(safe-area-inset-top))] max-sm:bottom-auto"
      >
        {toasts.map((toast) => (
          <ToastCard
            key={toast.key}
            toast={toast}
            host={modal}
            onClose={() => dismiss(toast.key)}
          />
        ))}
      </div>
    </>,
    root,
  );
}

function ToastCard({
  toast,
  host,
  onClose,
}: {
  toast: Toast;
  // The dialog the stack sits in, or null for the page.
  host: Element | null;
  onClose: () => void;
}) {
  // The timer waits while the pointer or the keyboard focus is on the toast.
  const [hover, setHover] = useState(false);
  const [focus, setFocus] = useState(false);
  const held = hover || focus;
  // A dialog that closes or unmounts takes the focus away without a blur
  // event, which would leave the toast held for good. Look again whenever
  // the stack changes its place.
  const card = useRef<HTMLDivElement>(null);
  useEffect(() => {
    setFocus(card.current?.contains(document.activeElement) === true);
  }, [host]);
  const close = useRef(onClose);
  close.current = onClose;
  const ms = lifetime(toast);
  useEffect(() => {
    if (ms === 0 || held) return undefined;
    const timer = setTimeout(() => close.current(), ms);
    return () => clearTimeout(timer);
  }, [ms, held]);
  const { action } = toast;
  return (
    <div
      ref={card}
      className="pointer-events-auto flex max-w-[min(480px,100%)] items-center gap-2.25 rounded-[10px] bg-ink py-1.5 pr-1.5 pl-4 text-[13px] font-medium text-on-ink shadow-[0_6px_30px_#0003]"
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      onFocus={() => setFocus(true)}
      // Focus moving between the toast's own buttons stays inside it.
      onBlur={(e) => setFocus(e.currentTarget.contains(e.relatedTarget))}
    >
      {toast.failed === true ? (
        <TriangleAlert size={15} className="shrink-0" />
      ) : (
        <Check size={15} className="shrink-0" />
      )}
      <span className="min-w-0 py-1.25 wrap-break-word">{toast.text}</span>
      {action && (
        <button
          className={cx(toastButton, "px-2.5 underline underline-offset-3")}
          onClick={() => {
            onClose();
            action.run();
          }}
        >
          {action.label}
        </button>
      )}
      <button
        className={cx(toastButton, "w-8 max-sm:w-11")}
        aria-label="Dismiss"
        onClick={onClose}
      >
        <X size={15} />
      </button>
    </div>
  );
}

// Buttons on the toast's ink fill. Phones get a full-size touch target.
const toastButton =
  "inline-flex h-8 shrink-0 items-center justify-center rounded-[7px] whitespace-nowrap hover:bg-[color-mix(in_srgb,var(--color-on-ink)_12%,transparent)] max-sm:h-11";
