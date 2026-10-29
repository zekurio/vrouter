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
import { RoleBadge } from "./Badge";
import { Private } from "./Privacy";
import { themeIcons, themeLabels, themes, type Theme } from "./theme";
import { cx, tab, tabs } from "./ui";

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
export function Avatar({
  user,
  size,
  className = "",
}: {
  user: SessionUser;
  size: number;
  className?: string;
}) {
  const [failed, setFailed] = useState("");
  const shape = "inline-flex shrink-0 items-center justify-center rounded-full";
  if (user.picture !== "" && failed !== user.picture) {
    return (
      <img
        // A faint edge keeps white pictures apart from a light background.
        className={cx(
          shape,
          "bg-chip object-cover outline-1 -outline-offset-1 outline-text/12",
          className,
        )}
        style={{ width: size, height: size }}
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
      className={cx(
        shape,
        "bg-[color-mix(in_srgb,var(--color-accent)_16%,var(--color-surface))] font-semibold tracking-[0.2px] text-accent select-none",
        className,
      )}
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
      className="relative"
      ref={root}
      onBlur={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget)) setOpen(null);
      }}
    >
      <button
        ref={trigger}
        type="button"
        className="group flex rounded-full p-0.5"
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
        <Avatar
          user={props.user}
          size={30}
          className="group-hover:ring-2 group-hover:ring-line group-aria-expanded:ring-2 group-aria-expanded:ring-line"
        />
      </button>
      {open !== null && (
        // Keep focus in the menu while the pointer works it. Safari does not
        // focus a button on click, so the menu would otherwise close.
        // oxlint-disable-next-line jsx-a11y/no-static-element-interactions -- only stops the pointer from moving focus; the menu handles keys
        <div
          className="absolute top-[calc(100%+8px)] right-0 z-30 w-70 max-w-[calc(100vw-24px)] animate-select-in rounded-xl border border-line bg-surface p-1.5 shadow-[0_12px_40px_#0008] [--color-well:var(--color-field)] light:shadow-[0_12px_32px_#0000001f]"
          onMouseDown={(e) => e.preventDefault()}
        >
          <UserCard user={props.user} />
          <div
            ref={menu}
            id={`${id}-menu`}
            role="menu"
            tabIndex={-1}
            className="focus:outline-none"
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
    e.target instanceof HTMLElement &&
    e.target.closest('[aria-label="Theme"]') !== null;
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

// The role sits beside the name, so the address below gets the full width,
// and so does its reveal, which stays inside that column.
function UserCard({ user }: { user: SessionUser }) {
  return (
    <div className="grid grid-cols-[auto_minmax(0,1fr)] items-center gap-3 px-2 pt-2 pb-3.5">
      <Avatar user={user} size={40} />
      <div className="@container">
        <div className="flex items-center justify-between gap-3">
          <strong className="truncate text-[13.5px] font-semibold">
            {user.name || "Signed in"}
          </strong>
          <RoleBadge role={user.role} />
        </div>
        {user.email !== "" && (
          // A hidden address has no text to cut short, and clipping would
          // hide its reveal.
          <span className="mt-1 block truncate text-[12px] text-muted [--reveal-max:calc(100cqw+12px)] has-[button]:overflow-visible">
            <Private peek>{user.email}</Private>
          </span>
        )}
      </div>
    </div>
  );
}

const section = "border-t border-border pt-1.5";
const item =
  "flex h-8.5 w-full items-center gap-2.5 rounded-[7px] px-2.5 text-left text-[13px] text-subtle hover:bg-chip hover:text-text focus-visible:bg-chip focus-visible:text-text focus-visible:outline-none light:hover:bg-control light:focus-visible:bg-control";
const itemIcon = "inline-flex text-muted";

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
      <div className={section} role="none">
        <div
          className="flex items-center justify-between gap-3 pr-1 pb-1 pl-2.5 text-[13px] text-subtle"
          role="group"
          aria-label="Theme"
        >
          <span aria-hidden="true">Theme</span>
          <div className={tabs(true)}>
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
                  className={tab(theme === t, "sm")}
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
          className={item}
          onClick={() => setHideEmails(!hideEmails)}
        >
          <span className={itemIcon} aria-hidden="true">
            <EyeOff size={16} />
          </span>
          Hide email addresses
          {hideEmails && (
            <Check
              size={15}
              className="ml-auto text-accent"
              aria-hidden="true"
            />
          )}
        </button>
        <MenuItem icon={<CircleHelp size={16} />} onClick={() => run(onHelp)}>
          API endpoints
        </MenuItem>
      </div>
      <div className={cx(section, "mt-1.5")} role="none">
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
      className={item}
      onClick={onClick}
    >
      <span className={itemIcon} aria-hidden="true">
        {icon}
      </span>
      {children}
    </button>
  );
}
