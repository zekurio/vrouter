import {
  Boxes,
  ChartColumn,
  KeyRound,
  LayoutGrid,
  UserCog,
  Users,
} from "lucide-react";
import type { SessionUser } from "./api";
import { nextThemes, themeIcons, useTheme } from "./theme";
import { cx, iconButton, lifted } from "./ui";
import { UserMenu, type UserMenuProps } from "./UserMenu";
import { pagesFor, type Page } from "./Workspace";

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
      className="h-6.5 w-6.5 max-sm:w-5.75"
      width="30"
      height="30"
      viewBox="4 4 24 24"
      fill="none"
      strokeWidth="3.75"
      strokeLinecap="round"
      aria-hidden="true"
    >
      <path d="M7.25 8.5 16 23.5" stroke="currentColor" />
      <path d="M24.75 8.5 19.94 16.75" stroke="var(--color-accent)" />
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
  actions: Omit<HeaderActionProps, "user" | "theme" | "setTheme">;
}) {
  // Only the header's controls change the theme, so it lives here.
  const { theme, setTheme } = useTheme();
  return (
    // Below 1100px the navigation takes a second row, and on phones it is a
    // bar pinned to the bottom edge.
    <header
      className={cx(
        "mx-auto flex h-17 max-w-[1440px] items-center gap-7 px-7.5 max-2xl:grid max-2xl:h-auto max-2xl:grid-cols-[auto_minmax(0,1fr)_auto] max-2xl:gap-x-6 max-2xl:gap-y-3 max-2xl:pt-5 max-xl:px-6 max-sm:px-4 max-xs:grid-cols-[minmax(0,1fr)_auto]",
        !user && "max-2xl:pb-5",
      )}
    >
      <a
        className="flex items-center gap-2.5 text-[16px] tracking-[-0.3px] whitespace-nowrap max-sm:gap-2"
        href="#overview"
        onClick={() => navigate("Overview")}
      >
        <RouterLogo />
        <span>
          <strong className="font-[650]">vrouter</strong>
        </span>
      </a>
      {user && (
        <nav
          className="flex h-full items-center gap-0.5 max-2xl:col-span-full max-2xl:row-start-2 max-2xl:h-auto max-2xl:overflow-x-auto max-2xl:[scrollbar-width:none] max-sm:fixed max-sm:inset-x-0 max-sm:bottom-0 max-sm:z-15 max-sm:grid max-sm:auto-cols-[minmax(0,1fr)] max-sm:grid-flow-col max-sm:gap-1 max-sm:overflow-visible max-sm:border-t max-sm:border-border max-sm:bg-bg max-sm:px-2 max-sm:py-1.5"
          aria-label="Main navigation"
        >
          {pagesFor(user.role).map((item) => {
            const Icon = pageIcons[item];
            return (
              <a
                key={item}
                href={`#${item.toLowerCase()}`}
                className={cx(
                  "flex h-8.5 items-center gap-2 rounded-lg px-3 font-medium max-2xl:shrink-0 max-sm:h-12.5 max-sm:flex-col max-sm:justify-center max-sm:gap-1.25 max-sm:p-0 max-sm:text-[11px]",
                  page === item ? lifted : "text-muted hover:text-text",
                )}
                aria-current={page === item ? "page" : undefined}
                onClick={() => navigate(item)}
              >
                <Icon size={19} className="size-3.75" aria-hidden="true" />
                {item}
              </a>
            );
          })}
        </nav>
      )}
      <HeaderActions
        {...actions}
        theme={theme}
        setTheme={setTheme}
        user={user}
      />
    </header>
  );
}

const headerActions =
  "ml-auto flex items-center gap-1 max-2xl:col-start-3 max-2xl:row-start-1 max-sm:gap-0.5 max-xs:col-start-2";

type HeaderActionProps = Omit<UserMenuProps, "user"> & {
  user: SessionUser | null;
};

// Signed in, every action lives in the user menu. The sign-in screen keeps
// only the theme.
function HeaderActions({ user, ...actions }: HeaderActionProps) {
  if (user) {
    return (
      <div className={headerActions}>
        <UserMenu user={user} {...actions} />
      </div>
    );
  }
  const { theme, setTheme } = actions;
  const nextTheme = nextThemes[theme];
  const ThemeIcon = themeIcons[theme];
  return (
    <div className={headerActions}>
      <button
        className={iconButton()}
        aria-label={`Theme: ${theme}. Switch to ${nextTheme}`}
        title={`Theme: ${theme}`}
        onClick={() => setTheme(nextTheme)}
      >
        <ThemeIcon size={16} />
      </button>
    </div>
  );
}
