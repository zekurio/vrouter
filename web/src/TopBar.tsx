import {
  Boxes,
  ChartColumn,
  CircleHelp,
  Eye,
  EyeOff,
  KeyRound,
  LayoutGrid,
  LogOut,
  Monitor,
  Moon,
  Sun,
  Users,
} from "lucide-react";
import type { Gateway } from "./api";
import { GatewaySwitcher } from "./GatewaySwitcher";
import { pages, type Page } from "./Workspace";

const pageIcons = {
  Overview: LayoutGrid,
  Models: Boxes,
  Accounts: Users,
  Keys: KeyRound,
  Usage: ChartColumn,
} satisfies Record<Page, unknown>;
const themes = ["system", "dark", "light"] as const;
export type Theme = (typeof themes)[number];
const themeIcons = { system: Monitor, dark: Moon, light: Sun };
const nextThemes: Record<Theme, Theme> = {
  system: "dark",
  dark: "light",
  light: "system",
};

function RouterLogo() {
  return (
    <svg
      width="30"
      height="30"
      viewBox="4 4 24 24"
      fill="none"
      strokeWidth="3.75"
      strokeLinecap="round"
      aria-hidden="true"
    >
      <path d="M7.25 8.5 16 23.5" stroke="currentColor" />
      <path d="M24.75 8.5 19.94 16.75" stroke="var(--accent)" />
    </svg>
  );
}

export function TopBar({
  gateway,
  gateways,
  ready,
  page,
  navigate,
  onSelect,
  actions,
}: {
  // The open gateway, if any.
  gateway: Gateway | undefined;
  gateways: Gateway[];
  // Signed in, with the gateway list loaded.
  ready: boolean;
  page: Page;
  navigate: (page: Page) => void;
  onSelect: (id: string) => void;
  actions: HeaderActionProps;
}) {
  return (
    <header className={`topbar ${gateway ? "" : "is-bare"}`}>
      <a
        className="wordmark"
        href="#overview"
        onClick={() => navigate("Overview")}
      >
        <RouterLogo />
        <span>
          <strong>vrouter</strong>
        </span>
      </a>
      {ready && gateways.length > (gateway ? 1 : 0) && (
        <GatewaySwitcher
          gateways={gateways}
          selected={gateway ? gateway.id : null}
          onSelect={onSelect}
        />
      )}
      {gateway && (
        <nav aria-label="Main navigation">
          {pages.map((item) => {
            const Icon = pageIcons[item];
            return (
              <a
                key={item}
                href={`#${item.toLowerCase()}`}
                className={page === item ? "active" : ""}
                aria-current={page === item ? "page" : undefined}
                onClick={() => navigate(item)}
              >
                <Icon size={19} aria-hidden="true" />
                {item}
              </a>
            );
          })}
        </nav>
      )}
      <HeaderActions {...actions} />
    </header>
  );
}

type HeaderActionProps = {
  hideEmails: boolean;
  setHideEmails: (hidden: boolean) => void;
  theme: Theme;
  setTheme: (theme: Theme) => void;
  onHelp: () => void;
  // Unset when there is no session this page can end.
  onSignOut: (() => void) | undefined;
};

function HeaderActions({
  hideEmails,
  setHideEmails,
  theme,
  setTheme,
  onHelp,
  onSignOut,
}: HeaderActionProps) {
  const nextTheme = nextThemes[theme];
  const ThemeIcon = themeIcons[theme];
  return (
    <div className="header-actions">
      <button
        className="icon-button"
        aria-label={
          hideEmails ? "Show email addresses" : "Hide email addresses"
        }
        title={hideEmails ? "Show email addresses" : "Hide email addresses"}
        onClick={() => setHideEmails(!hideEmails)}
      >
        {hideEmails ? <EyeOff size={16} /> : <Eye size={16} />}
      </button>
      <button
        className="icon-button"
        aria-label={`Theme: ${theme}. Switch to ${nextTheme}`}
        title={`Theme: ${theme}`}
        onClick={() => setTheme(nextTheme)}
      >
        <ThemeIcon size={16} />
      </button>
      <button
        className="icon-button"
        aria-label="Show API endpoints"
        title="API endpoints"
        onClick={onHelp}
      >
        <CircleHelp size={16} />
      </button>
      {onSignOut && (
        <button
          className="icon-button"
          aria-label="Sign out"
          title="Sign out"
          onClick={onSignOut}
        >
          <LogOut size={16} />
        </button>
      )}
    </div>
  );
}
