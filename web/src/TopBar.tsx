import {
  Boxes,
  ChartColumn,
  KeyRound,
  LayoutGrid,
  UserCog,
  Users,
} from "lucide-react";
import type { SessionUser } from "./api";
import { nextThemes, themeIcons } from "./theme";
import { UserMenu, type UserMenuProps } from "./UserMenu";
import { pagesFor, type Page } from "./Workspace";

export type { Theme } from "./theme";

const pageIcons = {
  Overview: LayoutGrid,
  Models: Boxes,
  Accounts: Users,
  Keys: KeyRound,
  Usage: ChartColumn,
  Users: UserCog,
} satisfies Record<Page, unknown>;

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
  user,
  page,
  navigate,
  actions,
}: {
  // Set once signed in, so the pages can open.
  user: SessionUser | null;
  page: Page;
  navigate: (page: Page) => void;
  actions: Omit<HeaderActionProps, "user">;
}) {
  return (
    <header className={`topbar ${user ? "" : "is-bare"}`}>
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
      {user && (
        <nav aria-label="Main navigation">
          {pagesFor(user.role).map((item) => {
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
      <HeaderActions {...actions} user={user} />
    </header>
  );
}

type HeaderActionProps = Omit<UserMenuProps, "user"> & {
  user: SessionUser | null;
};

// Signed in, every action lives in the user menu. The sign-in screen keeps
// only the theme.
function HeaderActions({ user, ...actions }: HeaderActionProps) {
  if (user) {
    return (
      <div className="header-actions">
        <UserMenu user={user} {...actions} />
      </div>
    );
  }
  const { theme, setTheme } = actions;
  const nextTheme = nextThemes[theme];
  const ThemeIcon = themeIcons[theme];
  return (
    <div className="header-actions">
      <button
        className="icon-button"
        aria-label={`Theme: ${theme}. Switch to ${nextTheme}`}
        title={`Theme: ${theme}`}
        onClick={() => setTheme(nextTheme)}
      >
        <ThemeIcon size={16} />
      </button>
    </div>
  );
}
