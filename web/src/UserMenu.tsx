import {
  useEffect,
  useId,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import { Check, CircleHelp, EyeOff, LogOut } from "lucide-react";
import type { SessionUser } from "./api";
import { Private } from "./Privacy";
import { themeIcons, themeLabels, themes, type Theme } from "./theme";

const itemSelector =
  '[role="menuitem"], [role="menuitemradio"], [role="menuitemcheckbox"]';

const segmenter = new Intl.Segmenter();
const firstLetter = (word: string) =>
  Array.from(segmenter.segment(word), (s) => s.segment)[0] ?? "";

// Up to two letters: the first and last words of the name, or the first
// letter of the address.
function initialsOf(label: string) {
  const words = label.split(/[\s@._-]+/u).filter((w) => w !== "");
  const last = words.length > 1 && !label.includes("@") ? words.at(-1) : "";
  return (firstLetter(words[0] ?? "") || "?") + firstLetter(last ?? "");
}

// The identity provider's profile picture, or initials when there is none or
// it fails to load. It loads without a referrer, so the provider never learns
// the dashboard's address.
export function Avatar({ user, size }: { user: SessionUser; size: number }) {
  const [failed, setFailed] = useState("");
  if (user.picture !== "" && failed !== user.picture) {
    return (
      <img
        className="avatar"
        src={user.picture}
        alt=""
        width={size}
        height={size}
        referrerPolicy="no-referrer"
        onError={() => setFailed(user.picture)}
      />
    );
  }
  return (
    <span
      className="avatar is-initials"
      style={{ width: size, height: size, fontSize: size * 0.4 }}
      aria-hidden="true"
    >
      {initialsOf(user.name || user.email).toUpperCase()}
    </span>
  );
}

export type UserMenuProps = {
  user: SessionUser;
  hideEmails: boolean;
  setHideEmails: (hidden: boolean) => void;
  theme: Theme;
  setTheme: (theme: Theme) => void;
  onHelp: () => void;
  onSignOut: () => void;
};

// Where focus goes when the menu opens. A click focuses the menu itself, so
// no item looks chosen; the keyboard lands on the first or last item.
type Opening = "menu" | "first" | "last";

// Menu button pattern: opening moves focus into the menu, arrow keys move
// between items, and Escape returns focus to the avatar.
export function UserMenu(props: UserMenuProps) {
  const id = useId();
  const root = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const menu = useRef<HTMLDivElement>(null);
  const [open, setOpen] = useState<Opening | null>(null);

  useEffect(() => {
    if (open === null) return undefined;
    const items = [
      ...(menu.current?.querySelectorAll<HTMLElement>(itemSelector) ?? []),
    ];
    const start = { menu: menu.current, first: items[0], last: items.at(-1) };
    start[open]?.focus();
    const close = (e: PointerEvent) => {
      const inside =
        e.target instanceof Node && root.current?.contains(e.target) === true;
      if (!inside) setOpen(null);
    };
    document.addEventListener("pointerdown", close);
    return () => document.removeEventListener("pointerdown", close);
  }, [open]);

  const dismiss = () => {
    setOpen(null);
    trigger.current?.focus();
  };
  // Focus returns to the avatar before the action, so closing a dialog the
  // action opens lands back here.
  const run = (action: () => void) => {
    dismiss();
    action();
  };

  return (
    <div
      className="user-menu"
      ref={root}
      onBlur={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget)) setOpen(null);
      }}
    >
      <button
        ref={trigger}
        type="button"
        className="user-menu-trigger"
        aria-label="Account menu"
        aria-haspopup="menu"
        aria-expanded={open !== null}
        aria-controls={`${id}-menu`}
        // A keyboard press reports no clicks in detail.
        onClick={(e) => {
          if (open === null) setOpen(e.detail === 0 ? "first" : "menu");
          else setOpen(null);
        }}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown" || e.key === "ArrowUp") {
            e.preventDefault();
            setOpen(e.key === "ArrowDown" ? "first" : "last");
          }
        }}
      >
        <Avatar user={props.user} size={30} />
      </button>
      {open !== null && (
        // Keep focus in the menu while the pointer works it. Safari does not
        // focus a button on click, so the menu would otherwise close.
        // oxlint-disable-next-line jsx-a11y/no-static-element-interactions -- only stops the pointer from moving focus; the menu handles keys
        <div
          className="user-menu-popover"
          onMouseDown={(e) => e.preventDefault()}
        >
          <UserCard user={props.user} />
          <div
            ref={menu}
            id={`${id}-menu`}
            role="menu"
            tabIndex={-1}
            aria-label="Account"
            onKeyDown={(e) => moveFocus(e, dismiss)}
          >
            <MenuItems {...props} run={run} />
          </div>
        </div>
      )}
    </div>
  );
}

function moveFocus(e: KeyboardEvent<HTMLDivElement>, escape: () => void) {
  if (e.key === "Escape") {
    e.preventDefault();
    escape();
    return;
  }
  const items = [
    ...e.currentTarget.querySelectorAll<HTMLElement>(itemSelector),
  ];
  const at = items.findIndex((item) => item === document.activeElement);
  // Left and right only move within the theme choices.
  const inRow =
    e.target instanceof HTMLElement && e.target.closest(".theme-row") !== null;
  const step: Record<string, number> = {
    ArrowDown: at + 1,
    ArrowUp: at - 1,
    ...(inRow ? { ArrowRight: at + 1, ArrowLeft: at - 1 } : {}),
    Home: 0,
    End: items.length - 1,
  };
  const target = step[e.key];
  if (target === undefined) return;
  e.preventDefault();
  items.at(target % items.length)?.focus();
}

function UserCard({ user }: { user: SessionUser }) {
  return (
    <div className="user-card">
      <Avatar user={user} size={40} />
      <div>
        <strong>{user.name || "Signed in"}</strong>
        {user.email !== "" && (
          <span>
            <Private>{user.email}</Private>
          </span>
        )}
      </div>
      <span className={`role-badge ${user.role}`}>
        {user.role === "admin" ? "Admin" : "Member"}
      </span>
    </div>
  );
}

function MenuItems({
  hideEmails,
  setHideEmails,
  theme,
  setTheme,
  onHelp,
  onSignOut,
  run,
}: Omit<UserMenuProps, "user"> & { run: (action: () => void) => void }) {
  return (
    <>
      <div className="user-menu-section" role="none">
        <div className="theme-row" role="group" aria-label="Theme">
          <span aria-hidden="true">Theme</span>
          <div className="tabs">
            {themes.map((t) => {
              const Icon = themeIcons[t];
              return (
                <button
                  key={t}
                  type="button"
                  role="menuitemradio"
                  tabIndex={-1}
                  aria-checked={theme === t}
                  aria-label={themeLabels[t]}
                  title={themeLabels[t]}
                  className={theme === t ? "selected" : ""}
                  onClick={() => setTheme(t)}
                >
                  <Icon size={15} aria-hidden="true" />
                </button>
              );
            })}
          </div>
        </div>
        <button
          type="button"
          role="menuitemcheckbox"
          tabIndex={-1}
          aria-checked={hideEmails}
          className="user-menu-item"
          onClick={() => setHideEmails(!hideEmails)}
        >
          <span aria-hidden="true">
            <EyeOff size={16} />
          </span>
          Hide email addresses
          {hideEmails && <Check size={15} aria-hidden="true" />}
        </button>
        <MenuItem icon={<CircleHelp size={16} />} onClick={() => run(onHelp)}>
          API endpoints
        </MenuItem>
      </div>
      <div className="user-menu-section" role="none">
        <MenuItem icon={<LogOut size={16} />} onClick={() => run(onSignOut)}>
          Sign out
        </MenuItem>
      </div>
    </>
  );
}

function MenuItem({
  icon,
  onClick,
  children,
}: {
  icon: ReactNode;
  onClick: () => void;
  children: string;
}) {
  return (
    <button
      type="button"
      role="menuitem"
      tabIndex={-1}
      className="user-menu-item"
      onClick={onClick}
    >
      <span aria-hidden="true">{icon}</span>
      {children}
    </button>
  );
}
