import { useCallback, useEffect, useRef, useState } from "react";
import { Plus, RefreshCw } from "lucide-react";
import { AccountsPage, type Account } from "./AccountsPage";
import {
  createClient,
  errorMessage,
  isStale,
  statusOf,
  type Role,
  type SessionUser,
} from "./api";
import { KeysPage } from "./KeysPage";
import { ModelsPage } from "./ModelsPage";
import type { Model } from "./models";
import { Overview } from "./Overview";
import { UsagePage } from "./UsagePage";
import { UsersPage } from "./UsersPage";

type State = {
  observedAt: string;
  models: Model[];
  accounts: Account[];
  warnings: string[];
};
export const pages = [
  "Overview",
  "Models",
  "Accounts",
  "Keys",
  "Usage",
  "Users",
] as const;
export type Page = (typeof pages)[number];
// Pages only admins open.
const adminPages: ReadonlySet<Page> = new Set(["Users"]);
export const pagesFor = (role: Role) =>
  pages.filter((p) => role === "admin" || !adminPages.has(p));
const titles: Partial<Record<Page, string>> = {
  Overview: "Account pools",
  Keys: "API keys",
};

type Props = {
  page: Page;
  user: SessionUser;
  navigate: (page: Page) => void;
  publicUrl: string;
  onUnauthorized: () => void;
  copy: (value: string) => Promise<boolean>;
  notify: (text: string) => void;
  // True while there are unsaved model drafts.
  onDirty: (dirty: boolean) => void;
};

// Loads the models and accounts, polls them while the page is visible, and
// retries with a growing delay while vrouter cannot be reached.
function useWorkspaceState(client: ReturnType<typeof createClient>) {
  const [state, setState] = useState<State | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const refreshSequence = useRef(0);
  const refreshPending = useRef(false);
  const retry = useRef({ delay: 0, timer: 0 });

  const refresh = useCallback(
    // Named so the retry timer can call it again.
    async function load(background = false): Promise<void> {
      if (background && refreshPending.current) return;
      refreshPending.current = true;
      const sequence = ++refreshSequence.current;
      clearTimeout(retry.current.timer);
      setLoading(true);
      // A background refresh keeps the last error on screen until it succeeds.
      if (!background) setError("");
      try {
        const next = await client.request<State>("/api/state");
        if (sequence !== refreshSequence.current) return;
        retry.current.delay = 0;
        setError("");
        setState(next);
      } catch (err) {
        if (sequence !== refreshSequence.current || isStale(err)) return;
        if (statusOf(err) === 401) return;
        setError(errorMessage(err, "Could not load models and accounts."));
        // Try again soon instead of waiting for the next poll, backing off while
        // vrouter stays unreachable.
        const delay = Math.min(retry.current.delay * 2 || 3000, 60000);
        retry.current = {
          delay,
          timer: window.setTimeout(() => void load(true), delay),
        };
      } finally {
        if (sequence === refreshSequence.current) {
          setLoading(false);
          refreshPending.current = false;
        }
      }
    },
    [client],
  );
  useEffect(() => {
    void refresh();
    const poll = () => {
      if (document.visibilityState === "visible") void refresh(true);
    };
    const timer = window.setInterval(poll, 60000);
    document.addEventListener("visibilitychange", poll);
    return () => {
      clearInterval(timer);
      clearTimeout(retry.current.timer);
      document.removeEventListener("visibilitychange", poll);
      client.close();
    };
  }, [client, refresh]);

  return { state, loading, error, refresh };
}

// Every page behind sign-in. Signing out unmounts it, which closes its client.
export function Workspace({
  page,
  user,
  navigate,
  publicUrl,
  onUnauthorized,
  copy,
  notify,
  onDirty,
}: Props) {
  const unauthorized = useRef(onUnauthorized);
  unauthorized.current = onUnauthorized;
  const [client] = useState(() =>
    createClient({ onUnauthorized: () => unauthorized.current() }),
  );
  const { state, loading, error, refresh } = useWorkspaceState(client);
  const [pool, setPool] = useState("");
  const [connect, setConnect] = useState<string | null>(null);
  const [creatingKey, setCreatingKey] = useState(false);

  const addAccount = () => {
    navigate("Accounts");
    setConnect("");
  };
  const endpoint = `${publicUrl || location.origin}/v1`;
  const admin = user.role === "admin";

  return (
    <>
      <PageHeading
        page={page}
        state={state}
        loading={loading}
        onAddAccount={addAccount}
        onCreateKey={() => setCreatingKey(true)}
        onRefresh={() => void refresh()}
      />
      <WorkspaceNotices
        state={state}
        loading={loading}
        error={error}
        onRetry={() => void refresh()}
      />
      {page === "Overview" && state && (
        <Overview accounts={state.accounts} pool={pool} setPool={setPool} />
      )}
      {state && (
        <div hidden={page !== "Models"}>
          <ModelsPage
            models={state.models}
            request={client.request}
            active={page === "Models"}
            reloadKey={state.observedAt}
            endpoint={endpoint}
            copy={copy}
            notify={notify}
            onSaved={() => void refresh()}
            onDirty={onDirty}
            editable={admin}
            onAddAccount={addAccount}
          />
        </div>
      )}
      {page === "Accounts" && state && (
        <AccountsPage
          accounts={state.accounts}
          admin={admin}
          request={client.request}
          connect={connect}
          setConnect={setConnect}
          onChanged={() => void refresh()}
          notify={notify}
        />
      )}
      {page === "Keys" && state && (
        <KeysPage
          admin={admin}
          userId={user.id}
          request={client.request}
          reloadKey={state.observedAt}
          creating={creatingKey}
          onCreateClose={() => setCreatingKey(false)}
          onChanged={() => void refresh()}
          copy={copy}
          notify={notify}
        />
      )}
      {page === "Usage" && state && (
        <UsagePage
          admin={admin}
          request={client.request}
          reloadKey={state.observedAt}
        />
      )}
      {page === "Users" && admin && (
        <UsersPage user={user} request={client.request} notify={notify} />
      )}
    </>
  );
}

// Load errors, provider warnings, and the first load's spinner.
function WorkspaceNotices({
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
        <div className="notice error" role="alert">
          {error} <button onClick={onRetry}>Try again</button>
        </div>
      )}
      {state?.warnings.map((w) => (
        <div className="notice" key={w}>
          {w}
        </div>
      ))}
      {loading && !state && (
        <div className="loading" role="status" aria-label="Loading">
          <RefreshCw size={22} className="spinning" />
        </div>
      )}
    </>
  );
}

function PageHeading({
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
    <section className="page-heading">
      <div>
        <h1>{titles[page] ?? page}</h1>
        {(page === "Overview" || page === "Accounts") && state && (
          <span className="page-meta">
            {accounts} {accounts === 1 ? "account" : "accounts"}
          </span>
        )}
      </div>
      <div className="heading-actions">
        {page === "Overview" && (
          <button className="primary" disabled={!state} onClick={onAddAccount}>
            <Plus size={14} /> Add account
          </button>
        )}
        {page === "Keys" && (
          <button className="primary" disabled={!state} onClick={onCreateKey}>
            <Plus size={14} /> Create key
          </button>
        )}
        <button
          className="icon-button refresh"
          disabled={loading}
          aria-label="Refresh"
          title={
            state
              ? `Last refreshed ${new Date(state.observedAt).toLocaleTimeString()}`
              : "Refresh"
          }
          onClick={onRefresh}
        >
          <RefreshCw size={16} className={loading ? "spinning" : ""} />
        </button>
      </div>
    </section>
  );
}
