import { Plus, RefreshCw } from "lucide-react";
import {
  button,
  cx,
  iconButton,
  loading as loadingClass,
  notice,
  noticeAction,
} from "./ui";
import type { Page, State } from "./Workspace";

const titles: Partial<Record<Page, string>> = {
  Overview: "Account pools",
  Keys: "API keys",
};

// Load errors, provider warnings, and the first load's spinner.
export function WorkspaceNotices({
  state,
  loading,
  error,
  onRetry,
}: {
  state: State | null;
  loading: boolean;
  error: string;
  onRetry: () => void;
}) {
  return (
    <>
      {error && (
        <div className={cx(notice({ error: true }), "my-3.75")} role="alert">
          {error}{" "}
          <button className={noticeAction} onClick={onRetry}>
            Try again
          </button>
        </div>
      )}
      {state?.warnings.map((w) => (
        <div className={cx(notice(), "my-3.75")} key={w}>
          {w}
        </div>
      ))}
      {loading && !state && (
        <div className={loadingClass} role="status" aria-label="Loading">
          <RefreshCw size={22} className="animate-spin" />
        </div>
      )}
    </>
  );
}

export function PageHeading({
  page,
  state,
  loading,
  onAddAccount,
  onCreateKey,
  onRefresh,
}: {
  page: Page;
  state: State | null;
  loading: boolean;
  onAddAccount: () => void;
  onCreateKey: () => void;
  onRefresh: () => void;
}) {
  const accounts = state?.accounts.length ?? 0;
  return (
    <section className="mb-5 flex min-h-10 flex-wrap items-center justify-between gap-x-4 gap-y-3 max-sm:mb-4">
      <div className="flex flex-wrap items-baseline gap-x-3.5 gap-y-1">
        <h1 className="text-[26px] leading-[1.2] font-semibold tracking-[-0.6px] max-sm:text-[23px]">
          {titles[page] ?? page}
        </h1>
        {(page === "Overview" || page === "Accounts") && state && (
          <span className="text-[14px] text-muted">
            {accounts} {accounts === 1 ? "account" : "accounts"}
          </span>
        )}
      </div>
      <div className="flex items-center gap-2">
        {page === "Overview" && (
          <button
            className={button("primary", "lg")}
            disabled={!state}
            onClick={onAddAccount}
          >
            <Plus size={14} /> Add account
          </button>
        )}
        {page === "Keys" && (
          <button
            className={button("primary", "lg")}
            disabled={!state}
            onClick={onCreateKey}
          >
            <Plus size={14} /> Create key
          </button>
        )}
        <button
          className={iconButton("lg")}
          disabled={loading}
          aria-label="Refresh"
          title={
            state
              ? `Last refreshed ${new Date(state.observedAt).toLocaleTimeString()}`
              : "Refresh"
          }
          onClick={onRefresh}
        >
          <RefreshCw size={16} className={loading ? "animate-spin" : ""} />
        </button>
      </div>
    </section>
  );
}
