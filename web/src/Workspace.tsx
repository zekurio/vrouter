import { useEffect, useRef, useState, type CSSProperties } from "react";
import { Plus, RefreshCw } from "lucide-react";
import {
  AccountsPage,
  accountLabel,
  accountState,
  resetTime,
  type Account,
} from "./AccountsPage";
import {
  createClient,
  errorMessage,
  isStale,
  statusOf,
  type AuthMode,
  type Gateway,
} from "./api";
import { KeysPage } from "./KeysPage";
import { ModelsPage, type Model } from "./ModelsPage";
import { Private } from "./Privacy";
import {
  ProviderBrand as Brand,
  providerColor as color,
  providerLabel,
} from "./ProviderBrand";
import { UsagePage } from "./UsagePage";

type State = {
  mode: "live" | "unconfigured";
  connected: boolean;
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
  token: () => string;
  authMode: AuthMode;
  publicUrl: string;
  onUnauthorized: () => void;
  // The server no longer has this gateway, or no longer lets this user open it.
  onGone: () => void;
  copy: (value: string) => Promise<boolean>;
  notify: (text: string) => void;
};

// Everything that belongs to one gateway. The app mounts it with the gateway ID
// as its key, so switching gateways throws this state away and starts clean.
export function Workspace({
  gateway,
  page,
  navigate,
  token,
  authMode,
  publicUrl,
  onUnauthorized,
  onGone,
  copy,
  notify,
}: Props) {
  const handlers = useRef({ onUnauthorized, onGone });
  handlers.current = { onUnauthorized, onGone };
  const [client] = useState(() =>
    createClient({
      gateway: gateway.id,
      token,
      onUnauthorized: () => handlers.current.onUnauthorized(),
    }),
  );
  const [state, setState] = useState<State | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const refreshSequence = useRef(0);
  const refreshPending = useRef(false);
  const [pool, setPool] = useState("");
  const [connect, setConnect] = useState<string | null>(null);
  const [creatingKey, setCreatingKey] = useState(false);

  async function refresh(background = false) {
    if (background && refreshPending.current) return;
    refreshPending.current = true;
    const sequence = ++refreshSequence.current;
    setLoading(true);
    setError("");
    try {
      const next = await client.request<State>("/api/state");
      if (sequence !== refreshSequence.current) return;
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
    } finally {
      if (sequence === refreshSequence.current) {
        setLoading(false);
        refreshPending.current = false;
      }
    }
  }
  useEffect(() => {
    void refresh();
    const poll = () => {
      if (document.visibilityState === "visible") void refresh(true);
    };
    const timer = window.setInterval(poll, 60000);
    document.addEventListener("visibilitychange", poll);
    return () => {
      clearInterval(timer);
      document.removeEventListener("visibilitychange", poll);
      client.close();
    };
  }, [client]);

  const addAccount = () => {
    navigate("Accounts");
    setConnect("");
  };
  const providers = [
    ...new Set((state?.accounts || []).map((a) => a.provider)),
  ];
  // The pool columns double as the filter. An empty filter shows every pool.
  const filter = providers.length > 1 && providers.includes(pool) ? pool : "";
  const visibleAccounts = (state?.accounts || []).filter(
    (a) => !filter || a.provider === filter,
  );
  const live = state?.mode === "live";
  // One address for every gateway. The API key picks the gateway.
  const endpoint = `${publicUrl || location.origin}/v1`;

  return (
    <>
      <section className="page-heading">
        <div>
          <h1>{titles[page] ?? page}</h1>
        </div>
        <div className="heading-actions">
          {page === "Overview" && (
            <button className="secondary" disabled={!live} onClick={addAccount}>
              <Plus size={14} /> Add account
            </button>
          )}
          {page === "Keys" && (
            <button
              className="secondary"
              disabled={!live}
              onClick={() => setCreatingKey(true)}
            >
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
            onClick={() => void refresh()}
          >
            <RefreshCw size={16} className={loading ? "spinning" : ""} />
          </button>
        </div>
      </section>
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
        <div className="loading">
          <RefreshCw size={22} className="spinning" />
        </div>
      )}
      {page === "Overview" && state && (
        <>
          {providers.length > 0 ? (
            <section className="provider-summary" aria-label="Provider pools">
              {providers.map((p) => {
                const accounts = state.accounts.filter((a) => a.provider === p);
                const known = accounts.filter((a) => a.remaining !== null);
                const remaining = known.reduce(
                  (sum, a) => sum + (a.remaining || 0),
                  0,
                );
                return (
                  <article
                    className={`provider-column ${filter && filter !== p ? "dimmed" : ""}`}
                    key={p}
                    style={{ "--provider": color(p) } as CSSProperties}
                  >
                    <div className="provider-title">
                      <Brand provider={p} />
                      <h2>
                        {providers.length > 1 ? (
                          <button
                            className="pool-toggle"
                            aria-pressed={filter === p}
                            title={
                              filter === p
                                ? "Show all pools"
                                : `Show only ${providerLabel(p)} accounts`
                            }
                            onClick={() => setPool(filter === p ? "" : p)}
                          >
                            {providerLabel(p)}
                          </button>
                        ) : (
                          p
                        )}
                      </h2>
                      <span>
                        {accounts.length}{" "}
                        {accounts.length === 1 ? "account" : "accounts"}
                      </span>
                    </div>
                    <div className="allowance">
                      {known.length === accounts.length ? (
                        <>
                          <span>
                            {Math.round(remaining)}
                            <small>%</small>
                          </span>
                          {accounts.length > 1 && (
                            <span className="allowance-of">
                              of {accounts.length * 100}%
                            </span>
                          )}
                        </>
                      ) : (
                        <span className="unknown-allowance">Not reported</span>
                      )}
                    </div>
                    <p className="window-label">
                      {accounts.every((a) => a.window === "Weekly window")
                        ? "Weekly remaining"
                        : "Allowance remaining"}
                    </p>
                    <div className="segmented-progress">
                      {accounts.map((a) => (
                        <div key={a.id}>
                          {a.remaining !== null && (
                            <span style={{ width: `${a.remaining}%` }} />
                          )}
                        </div>
                      ))}
                    </div>
                  </article>
                );
              })}
            </section>
          ) : (
            <div className="empty">No accounts yet</div>
          )}
          {providers.length > 0 && (
            <>
              <section className="account-grid">
                {visibleAccounts.map((a) => (
                  <article
                    key={a.id}
                    className="account-card"
                    style={{ "--provider": color(a.provider) } as CSSProperties}
                  >
                    <div className="account-heading">
                      <div>
                        <h3>
                          <Private peek>{accountLabel(a)}</Private>
                        </h3>
                        <span className="plan">
                          {a.plan || providerLabel(a.provider)}
                        </span>
                        {accountState(a) && (
                          <span className={`account-state ${a.status}`}>
                            {accountState(a)}
                          </span>
                        )}
                      </div>
                      <Brand provider={a.provider} small />
                    </div>
                    {a.windows?.length ? (
                      <div className="quota-windows">
                        {a.windows.map((window) => (
                          <div className="quota-window" key={window.id}>
                            <div className="account-allowance">
                              <span>{window.label}</span>
                              <strong>{window.remaining}% left</strong>
                            </div>
                            <div className="progress">
                              <span style={{ width: `${window.remaining}%` }} />
                            </div>
                            {window.resetAt && (
                              <p
                                title={new Date(
                                  window.resetAt,
                                ).toLocaleString()}
                              >
                                {resetTime(window.resetAt)}
                              </p>
                            )}
                          </div>
                        ))}
                      </div>
                    ) : (
                      <>
                        <div className="account-allowance">
                          <span>{a.window}</span>
                          <strong>
                            {a.remaining === null
                              ? "Unknown"
                              : `${a.remaining}% left`}
                          </strong>
                        </div>
                        {a.remaining !== null && (
                          <div className="progress">
                            <span style={{ width: `${a.remaining}%` }} />
                          </div>
                        )}
                      </>
                    )}
                    {a.quotaError && (
                      <p className="quota-error">
                        <Private>{a.quotaError}</Private>
                      </p>
                    )}
                  </article>
                ))}
              </section>
            </>
          )}
        </>
      )}
      {state && (
        <div hidden={page !== "Models"}>
          <ModelsPage
            models={state.models}
            request={client.request}
            live={state.mode === "live"}
            reloadKey={state.observedAt}
            endpoint={endpoint}
            copy={copy}
            notify={notify}
            onSaved={() => void refresh()}
            onAddAccount={addAccount}
          />
        </div>
      )}
      {page === "Accounts" && state && (
        <AccountsPage
          accounts={state.accounts}
          request={client.request}
          live={state.mode === "live"}
          connect={connect}
          setConnect={setConnect}
          onChanged={() => void refresh()}
          notify={notify}
        />
      )}
      {page === "Keys" && state && (
        <KeysPage
          request={client.request}
          live={state.mode === "live"}
          reloadKey={state.observedAt}
          creating={creatingKey}
          onCreateClose={() => setCreatingKey(false)}
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
