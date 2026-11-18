import { useCallback, useEffect, useRef, useState } from "react";
import { AccountsPage, type Account } from "./AccountsPage";
import { createClient, errorMessage, isStale, statusOf } from "./api";
import { KeysPage } from "./KeysPage";
import { ModelsPage } from "./ModelsPage";
import type { Model } from "./models";
import { Overview } from "./Overview";
import { PageHeading, WorkspaceNotices } from "./PageHeading";
import type { Page } from "./router";
import { UsagePage } from "./UsagePage";

export type State = {
  observedAt: string;
  models: Model[];
  accounts: Account[];
  warnings: string[];
};

type Props = {
  page: Page;
  navigate: (page: Page) => void;
  publicUrl: string;
  onUnauthorized: () => void;
  copy: (value: string) => Promise<boolean>;
  notify: (text: string) => void;
  // True while there are unsaved model drafts.
  onDirty: (dirty: boolean) => void;
};

// Loads the models and accounts, polls them while the page is visible, and
// retries with a growing delay while vrouter cannot be reached. failures
// counts the loads that failed in a row.
function useWorkspaceState(client: ReturnType<typeof createClient>) {
  const [state, setState] = useState<State | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [failures, setFailures] = useState(0);
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
        setFailures(0);
        setState(next);
      } catch (err) {
        if (sequence !== refreshSequence.current || isStale(err)) return;
        if (statusOf(err) === 401) return;
        setError(errorMessage(err, "Could not load models and accounts."));
        setFailures((n) => n + 1);
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

  return { state, loading, error, failures, refresh };
}

// Every page behind sign-in. Signing out unmounts it, which closes its client.
export function Workspace({
  page,
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
  const { state, loading, error, failures, refresh } =
    useWorkspaceState(client);
  const [pool, setPool] = useState("");
  const [connect, setConnect] = useState<string | null>(null);
  const [creatingKey, setCreatingKey] = useState(false);

  const addAccount = () => {
    navigate("Accounts");
    setConnect("");
  };
  const endpoint = `${publicUrl || location.origin}/v1`;

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
        failures={failures}
        onRetry={() => void refresh()}
      />
      {page === "Overview" && state && (
        <Overview
          accounts={state.accounts}
          pool={pool}
          setPool={setPool}
          request={client.request}
          reloadKey={state.observedAt}
        />
      )}
      {state && (
        <div hidden={page !== "Models"}>
          <ModelsPage
            models={state.models}
            warnings={state.warnings}
            request={client.request}
            active={page === "Models"}
            reloadKey={state.observedAt}
            endpoint={endpoint}
            copy={copy}
            notify={notify}
            onSaved={() => void refresh()}
            onDirty={onDirty}
            onAddAccount={addAccount}
          />
        </div>
      )}
      {page === "Accounts" && state && (
        <AccountsPage
          accounts={state.accounts}
          request={client.request}
          connect={connect}
          setConnect={setConnect}
          onChanged={() => void refresh()}
          notify={notify}
        />
      )}
      {page === "Keys" && state && (
        <KeysPage
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
        <UsagePage request={client.request} reloadKey={state.observedAt} />
      )}
    </>
  );
}
