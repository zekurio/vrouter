import {
  useEffect,
  useId,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import { Check, CircleHelp, EyeOff, LogOut, Settings } from "lucide-react";
import { themeIcons, themeLabels, themes, type Theme } from "./theme";
import { cx, iconButton, tab, tabs } from "./ui";

const itemSelector =
  '[role="menuitem"], [role="menuitemradio"], [role="menuitemcheckbox"]';

export type SettingsMenuProps = {
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
// between items, and Escape returns focus to the button.
export function SettingsMenu(props: SettingsMenuProps) {
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
  // Focus returns to the button before the action, so closing a dialog the
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
        className={cx(iconButton(), "aria-expanded:bg-hover")}
        aria-label="Settings"
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
        <Settings size={17} />
      </button>
      {open !== null && (
        // Keep focus in the menu while the pointer works it. Safari does not
        // focus a button on click, so the menu would otherwise close.
        // oxlint-disable-next-line jsx-a11y/no-static-element-interactions -- only stops the pointer from moving focus; the menu handles keys
        <div
          className="absolute top-[calc(100%+8px)] right-0 z-30 w-70 max-w-[calc(100vw-24px)] animate-select-in rounded-xl border border-line bg-surface p-1.5 shadow-[0_12px_40px_#0008] [--color-well:var(--color-field)] light:shadow-[0_12px_32px_#0000001f]"
          onMouseDown={(e) => e.preventDefault()}
        >
          <div
            ref={menu}
            id={`${id}-menu`}
            role="menu"
            tabIndex={-1}
            className="focus:outline-none"
            aria-label="Settings"
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
}: SettingsMenuProps & { run: (action: () => void) => void }) {
  return (
    <>
      <div role="none">
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
