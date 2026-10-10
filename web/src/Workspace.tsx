import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type RefObject,
} from "react";
import { Plus, RefreshCw } from "lucide-react";
import { AccountsPage, type Account } from "./AccountsPage";
import {
  createClient,
  errorMessage,
  isStale,
  statusOf,
  type Gateway,
} from "./api";
import { KeysPage } from "./KeysPage";
import { ModelsPage } from "./ModelsPage";
import type { Model } from "./models";
import { Overview } from "./Overview";
import { UsagePage } from "./UsagePage";

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
] as const;
export type Page = (typeof pages)[number];
const titles: Partial<Record<Page, string>> = {
  Overview: "Account pools",
  Keys: "API keys",
};

type Props = {
  gateway: Gateway;
  page: Page;
  navigate: (page: Page) => void;
  publicUrl: string;
  onUnauthorized: () => void;
  // The server no longer has this gateway, or no longer lets this user open it.
  onGone: () => void;
  copy: (value: string) => Promise<boolean>;
  notify: (text: string) => void;
  // True while this gateway has unsaved model drafts.
  onDirty: (dirty: boolean) => void;
};

// Loads the gateway state, polls it while the page is visible, and retries
// with a growing delay while the gateway cannot be reached.
function useGatewayState(
  client: ReturnType<typeof createClient>,
  handlers: RefObject<{ onGone: () => void }>,
) {
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
        const status = statusOf(err);
        if (status === 401) return;
        if (status === 404) {
          setState(null);
          setError("This gateway no longer exists, or you can't open it.");
          handlers.current.onGone();
          return;
        }
        setError(errorMessage(err, "Could not load gateway data."));
        // Try again soon instead of waiting for the next poll, backing off while
        // the gateway stays unreachable.
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
    [client, handlers],
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

// Everything that belongs to one gateway. The app mounts it with the gateway ID
// as its key, so switching gateways throws this state away and starts clean.
export function Workspace({
  gateway,
  page,
  navigate,
  publicUrl,
  onUnauthorized,
  onGone,
  copy,
  notify,
  onDirty,
}: Props) {
  const handlers = useRef({ onUnauthorized, onGone });
  handlers.current = { onUnauthorized, onGone };
  const [client] = useState(() =>
    createClient({
      gateway: gateway.id,
      onUnauthorized: () => handlers.current.onUnauthorized(),
    }),
  );
  const { state, loading, error, refresh } = useGatewayState(client, handlers);
  const [pool, setPool] = useState("");
  const [connect, setConnect] = useState<string | null>(null);
  const [creatingKey, setCreatingKey] = useState(false);

  const addAccount = () => {
    navigate("Accounts");
    setConnect("");
  };
  // One address for every gateway. The API key picks the gateway.
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
      {error && (
        <div className="notice error" role="alert">
          {error} <button onClick={() => void refresh()}>Try again</button>
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
  return (
    <section className="page-heading">
      <div>
        <h1>{titles[page] ?? page}</h1>
      </div>
      <div className="heading-actions">
        {page === "Overview" && (
          <button
            className="secondary"
            disabled={!state}
            onClick={onAddAccount}
          >
            <Plus size={14} /> Add account
          </button>
        )}
        {page === "Keys" && (
          <button className="secondary" disabled={!state} onClick={onCreateKey}>
            <Plus size={14} /> Create key
          </button>
        )}
        <button
          className="icon-button refresh"
          disabled={loading}
          aria-label="Refresh gateway data"
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
