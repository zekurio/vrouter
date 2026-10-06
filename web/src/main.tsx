import { useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import {
  Check,
  Eye,
  EyeOff,
  LogOut,
  Moon,
  Plus,
  RefreshCw,
  Sun,
} from "lucide-react";
import "./style.css";
import "@fontsource-variable/dm-sans";
import {
  createClient,
  errorMessage,
  pickGateway,
  statusOf,
  type AuthInfo,
  type Gateway,
  type User,
} from "./api";
import { CreateGatewayDialog, GatewaySwitcher } from "./GatewaySwitcher";
import { type Theme } from "./SettingsPage";
import { PrivacyProvider, storedHideEmails, storeHideEmails } from "./Privacy";
import { pages, Workspace, type Page } from "./Workspace";

const pageOf = (hash: string) =>
  pages.find((p) => p.toLowerCase() === hash.slice(1)) || "Overview";
const storedTheme = (): Theme => {
  const value = localStorage.getItem("vrouter-theme");
  return value === "system" || value === "light" ? value : "dark";
};
// Only the gateway ID is remembered. Tokens and key secrets never touch storage.
const gatewayKey = "vrouter-gateway";
const storedGateway = () => {
  try {
    return localStorage.getItem(gatewayKey);
  } catch {
    return null;
  }
};
const storeGateway = (id: string | null) => {
  try {
    if (id) localStorage.setItem(gatewayKey, id);
    else localStorage.removeItem(gatewayKey);
  } catch {
    // Storage is unavailable; the choice lasts for this page load only.
  }
};
const created = (value: string) =>
  new Date(value).toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });

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
function App() {
  const [page, setPage] = useState<Page>(() => pageOf(location.hash));
  // "login" covers every signed-out state, "ready" every signed-in one.
  const [phase, setPhase] = useState<"loading" | "login" | "ready" | "error">(
    "loading",
  );
  const [auth, setAuth] = useState<AuthInfo | null>(null);
  const [user, setUser] = useState<User | null>(null);
  const [gateways, setGateways] = useState<Gateway[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [error, setError] = useState("");
  const [loginError, setLoginError] = useState("");
  const [token, setToken] = useState("");
  const [tokenForm, setTokenForm] = useState(false);
  const [creating, setCreating] = useState(false);
  // Mode of the open gateway, once its state has loaded.
  const [gatewayMode, setGatewayMode] = useState("");
  const bearer = useRef("");
  const sequence = useRef(0);
  // Several requests can report the same expired session.
  const ending = useRef(false);
  // Not scoped to a gateway: sign-in and the gateway list work without one.
  const [client] = useState(() =>
    createClient({ token: () => bearer.current }),
  );
  const [toast, setToast] = useState("");
  const [theme, setTheme] = useState<Theme>(storedTheme);
  const [systemLight, setSystemLight] = useState(
    () => matchMedia("(prefers-color-scheme: light)").matches,
  );
  const [hideEmails, setHideEmails] = useState(storedHideEmails);
  const light = theme === "system" ? systemLight : theme === "light";

  function signedOut(info: AuthInfo | null, message: string) {
    bearer.current = "";
    setAuth(info);
    setUser(null);
    setGateways([]);
    setSelected(null);
    setGatewayMode("");
    setCreating(false);
    if (message) setLoginError(message);
    setPhase("login");
  }
  // Reads who is signed in, then the gateways they can open. `tried` is set
  // when the user has just submitted an admin token.
  async function load(tried = false) {
    const current = ++sequence.current;
    setPhase("loading");
    setError("");
    let info: AuthInfo | null = null;
    const rejected = tried ? "That admin token was not accepted." : "";
    try {
      info = await client.request<AuthInfo>("/api/auth");
      if (current !== sequence.current) return;
      if (!info.user && info.mode !== "local") return signedOut(info, rejected);
      const list = await client.request<{ gateways: Gateway[]; user: User }>(
        "/api/gateways",
      );
      if (current !== sequence.current) return;
      const next = list.gateways || [];
      setAuth(info);
      setUser(list.user ?? info.user);
      setGateways(next);
      setSelected(pickGateway(next, storedGateway(), info.mode));
      setLoginError("");
      ending.current = false;
      setPhase("ready");
    } catch (err) {
      if (current !== sequence.current) return;
      if (info && statusOf(err) === 401) return signedOut(info, rejected);
      setError(errorMessage(err, "Could not load vrouter."));
      setPhase("error");
    }
  }
  // Keeps the open gateway unless the server stopped listing it.
  async function reloadGateways() {
    const current = sequence.current;
    try {
      const list = await client.request<{ gateways: Gateway[] }>(
        "/api/gateways",
      );
      if (current !== sequence.current) return;
      const next = list.gateways || [];
      setGateways(next);
      setSelected((id) => (id && next.some((g) => g.id === id) ? id : null));
    } catch {
      // The open gateway already shows its own error.
    }
  }
  useEffect(() => {
    void load();
    const change = () => setPage(pageOf(location.hash));
    const system = matchMedia("(prefers-color-scheme: light)");
    const scheme = () => setSystemLight(system.matches);
    window.addEventListener("hashchange", change);
    system.addEventListener("change", scheme);
    return () => {
      window.removeEventListener("hashchange", change);
      system.removeEventListener("change", scheme);
    };
  }, []);
  useEffect(() => {
    if (!toast) return;
    const id = setTimeout(() => setToast(""), 2500);
    return () => clearTimeout(id);
  }, [toast]);
  useEffect(() => {
    document.documentElement.dataset.theme = light ? "light" : "dark";
    localStorage.setItem("vrouter-theme", theme);
  }, [light, theme]);
  useEffect(() => storeHideEmails(hideEmails), [hideEmails]);

  const oidc = auth?.mode === "oidc";
  const canSignOut =
    phase === "ready" && (!!bearer.current || (oidc && !!user));
  async function signOut() {
    sequence.current++;
    const cookie = oidc && !bearer.current;
    bearer.current = "";
    if (cookie) {
      try {
        await client.request("/api/logout", "POST");
      } catch (err) {
        if (statusOf(err) !== 401) {
          setToast(errorMessage(err, "Could not sign out."));
          return;
        }
      }
    }
    setLoginError("");
    void load();
  }
  const sessionEnded = () => {
    if (ending.current) return;
    ending.current = true;
    setLoginError(
      bearer.current
        ? "The admin token is no longer accepted. Sign in again."
        : "Your session ended. Sign in again.",
    );
    bearer.current = "";
    void load();
  };
  const select = (id: string) => {
    storeGateway(id);
    setGatewayMode("");
    setSelected(id);
  };
  const navigate = (next: Page) => {
    setPage(next);
    location.hash = next.toLowerCase();
  };
  const copy = async (value: string) => {
    try {
      await navigator.clipboard.writeText(value);
      setToast("Copied to clipboard");
      return true;
    } catch {
      setToast("Clipboard unavailable. Select and copy the text.");
      return false;
    }
  };
  const gateway = gateways.find((g) => g.id === selected);
  const demo = gatewayMode === "demo";
  const showToken = !oidc || tokenForm || !auth?.providers.length;

  return (
    <PrivacyProvider value={hideEmails}>
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
        {phase === "ready" && gateways.length > 0 && (
          <GatewaySwitcher
            gateways={gateways}
            selected={gateway ? gateway.id : null}
            onSelect={select}
            onCreate={() => setCreating(true)}
            canCreate={!demo}
          />
        )}
        {gateway && (
          <nav aria-label="Main navigation">
            {pages.map((item) => (
              <a
                key={item}
                href={`#${item.toLowerCase()}`}
                className={page === item ? "active" : ""}
                aria-current={page === item ? "page" : undefined}
                onClick={() => navigate(item)}
              >
                {item}
              </a>
            ))}
          </nav>
        )}
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
            aria-label={light ? "Use dark theme" : "Use light theme"}
            onClick={() => setTheme(light ? "dark" : "light")}
          >
            {light ? <Moon size={16} /> : <Sun size={16} />}
          </button>
          {canSignOut && (
            <button
              className="icon-button"
              aria-label="Sign out"
              title="Sign out"
              onClick={() => void signOut()}
            >
              <LogOut size={16} />
            </button>
          )}
        </div>
      </header>
      <main>
        {phase === "loading" && (
          <div className="empty">
            <RefreshCw size={22} className="spinning" />
          </div>
        )}
        {phase === "error" && (
          <div className="notice error" role="alert">
            {error} <button onClick={() => void load()}>Try again</button>
          </div>
        )}
        {phase === "login" && auth && (
          <section className="login-panel">
            <h1>Sign in</h1>
            {loginError && (
              <div className="notice error" role="alert">
                {loginError}
              </div>
            )}
            {auth.mode === "local" ? (
              <>
                <p className="login-note">
                  This vrouter has no sign-in configured. It only answers a
                  browser running on the same machine, at a loopback address.
                </p>
                <button className="primary" onClick={() => void load()}>
                  Try again
                </button>
              </>
            ) : (
              <>
                {auth.providers.length > 0 && (
                  <div className="login-providers">
                    {auth.providers.map((p) => (
                      <a className="primary" key={p.id} href={p.loginUrl}>
                        Continue with {p.name}
                      </a>
                    ))}
                  </div>
                )}
                {showToken ? (
                  <form
                    onSubmit={(e) => {
                      e.preventDefault();
                      bearer.current = token;
                      setToken("");
                      setLoginError("");
                      void load(true);
                    }}
                  >
                    <label htmlFor="admin-token">Admin token</label>
                    <input
                      id="admin-token"
                      type="password"
                      value={token}
                      onChange={(e) => setToken(e.target.value)}
                      required
                      autoFocus={tokenForm}
                      autoComplete="current-password"
                    />
                    <button
                      className={
                        auth.providers.length ? "secondary outlined" : "primary"
                      }
                    >
                      Sign in with token
                    </button>
                  </form>
                ) : (
                  <button
                    className="text-button"
                    onClick={() => setTokenForm(true)}
                  >
                    Use the admin token instead
                  </button>
                )}
              </>
            )}
          </section>
        )}
        {phase === "ready" && !gateway && (
          <>
            <section className="page-heading">
              <div>
                <h1>Gateways</h1>
              </div>
              {gateways.length > 0 && (
                <div className="heading-actions">
                  <button
                    className="secondary"
                    onClick={() => setCreating(true)}
                  >
                    <Plus size={14} /> New gateway
                  </button>
                </div>
              )}
            </section>
            {gateways.length === 0 ? (
              <div className="empty">
                <h2>You have no gateways yet</h2>
                <p>
                  A gateway holds your provider accounts and the API keys that
                  give others access to them. Create one to start.
                </p>
                <button className="primary" onClick={() => setCreating(true)}>
                  <Plus size={14} /> Create gateway
                </button>
              </div>
            ) : (
              <>
                <p className="models-help gateway-help">
                  Choose a gateway to open. Each one keeps its own accounts,
                  models, API keys and request log.
                </p>
                <ul className="gateway-list">
                  {gateways.map((g) => (
                    <li key={g.id}>
                      <button onClick={() => select(g.id)}>
                        <strong>{g.name}</strong>
                        <span>
                          {g.ownerId === user?.id
                            ? "Yours"
                            : "Owned by another user"}
                        </span>
                        <span>Created {created(g.createdAt)}</span>
                      </button>
                    </li>
                  ))}
                </ul>
              </>
            )}
          </>
        )}
        {phase === "ready" && gateway && auth && (
          <Workspace
            key={gateway.id}
            gateway={gateway}
            page={page}
            navigate={navigate}
            token={() => bearer.current}
            user={user}
            authMode={auth.mode}
            canSignOut={canSignOut}
            onSignOut={() => void signOut()}
            onUnauthorized={sessionEnded}
            onGone={() => void reloadGateways()}
            onMode={setGatewayMode}
            copy={copy}
            notify={setToast}
            theme={theme}
            setTheme={setTheme}
          />
        )}
      </main>
      {creating && (
        <CreateGatewayDialog
          request={client.request}
          onCreated={(next) => {
            setGateways((list) => [...list, next]);
            setCreating(false);
            select(next.id);
            navigate("Overview");
            setToast("Gateway created");
          }}
          onClose={() => setCreating(false)}
        />
      )}
      {toast && (
        <div className="toast" role="status">
          <Check size={15} />
          {toast}
        </div>
      )}
    </PrivacyProvider>
  );
}
createRoot(document.getElementById("root")!).render(<App />);
