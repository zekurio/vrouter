import { useEffect, useRef, useState, type CSSProperties } from "react";
import { Plus, RefreshCw } from "lucide-react";
import { AccountsPage, resetTime, type Account } from "./AccountsPage";
import {
  createClient,
  errorMessage,
  isStale,
  statusOf,
  type AuthMode,
  type Gateway,
  type User,
} from "./api";
import { KeysPage } from "./KeysPage";
import { ModelsPage, type Model } from "./ModelsPage";
import { Private } from "./Privacy";
import {
  ProviderBrand as Brand,
  providerColor as color,
} from "./ProviderBrand";
import { RequestsPage } from "./RequestsPage";
import { SettingsPage, type Engine, type Theme } from "./SettingsPage";

type State = {
  mode: "demo" | "live" | "unconfigured";
  connected: boolean;
  observedAt: string;
  models: Model[];
  accounts: Account[];
  warnings: string[];
  engine: Engine;
};
export const pages = [
  "Overview",
  "Models",
  "Accounts",
  "Keys",
  "Requests",
  "Settings",
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
  user: User | null;
  authMode: AuthMode;
  canSignOut: boolean;
  onSignOut: () => void;
  onUnauthorized: () => void;
  // The server no longer has this gateway, or no longer lets this user open it.
  onGone: () => void;
  onMode: (mode: State["mode"]) => void;
  copy: (value: string) => Promise<boolean>;
  notify: (text: string) => void;
  theme: Theme;
  setTheme: (theme: Theme) => void;
};

// Everything that belongs to one gateway. The app mounts it with the gateway ID
// as its key, so switching gateways throws this state away and starts clean.
export function Workspace({
  gateway,
  page,
  navigate,
  token,
  user,
  authMode,
  canSignOut,
  onSignOut,
  onUnauthorized,
  onGone,
  onMode,
  copy,
  notify,
  theme,
  setTheme,
}: Props) {
  const handlers = useRef({ onUnauthorized, onGone, onMode });
  handlers.current = { onUnauthorized, onGone, onMode };
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
  const [pool, setPool] = useState("All pools");
  const [connect, setConnect] = useState<string | null>(null);

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
      handlers.current.onMode(next.mode);
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
  const visibleAccounts = (state?.accounts || []).filter(
    (a) => pool === "All pools" || a.provider === pool,
  );
  const demo = state?.mode === "demo";
  const live = state?.mode === "live";
  // One address for every gateway. The API key picks the gateway.
  const endpoint = `${location.origin}/v1`;

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
      {demo && (
        <div className="notice">
          Demo data. Accounts, API keys and gateways can't be changed.
        </div>
      )}
      {state?.warnings.map((w) => (
        <div className="notice" key={w}>
          {w}
        </div>
      ))}
      {loading && !state && (
        <div className="empty">
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
                    className="provider-column"
                    key={p}
                    style={{ "--provider": color(p) } as CSSProperties}
                  >
                    <div className="provider-title">
                      <Brand provider={p} />
                      <h2>{p}</h2>
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
            <div className="empty">
              <h2>No accounts connected</h2>
            </div>
          )}
          {providers.length > 0 && (
            <>
              <div className="section-toolbar">
                <div className="tabs" aria-label="Filter account pools">
                  {["All pools", ...providers].map((p) => (
                    <button
                      aria-pressed={pool === p}
                      className={pool === p ? "selected" : ""}
                      key={p}
                      onClick={() => setPool(p)}
                    >
                      {p !== "All pools" && (
                        <span
                          className="tiny-dot"
                          style={{ background: color(p) }}
                        />
                      )}
                      {p}
                    </button>
                  ))}
                </div>
              </div>
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
                          <span
                            className={`account-state ${a.status}`}
                            title={a.status === "ready" ? "Active" : a.status}
                            aria-label={
                              a.status === "ready" ? "Active" : a.status
                            }
                          />
                          <Private>{a.name}</Private>
                        </h3>
                        <span className="plan">{a.plan || a.provider}</span>
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
          demo={state.mode === "demo"}
          reloadKey={state.observedAt}
          endpoint={endpoint}
          copy={copy}
          notify={notify}
        />
      )}
      {page === "Requests" && state && (
        <RequestsPage
          request={client.request}
          demo={state.mode === "demo"}
          reloadKey={state.observedAt}
        />
      )}
      {page === "Settings" && state && (
        <SettingsPage
          mode={state.mode}
          engine={state.engine}
          models={state.models.length}
          accounts={state.accounts.length}
          endpoint={endpoint}
          copy={(value) => void copy(value)}
          theme={theme}
          setTheme={setTheme}
          gateway={gateway}
          user={user}
          authMode={authMode}
          canSignOut={canSignOut}
          onSignOut={onSignOut}
        />
      )}
    </>
  );
}
