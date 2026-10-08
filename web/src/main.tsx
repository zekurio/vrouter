import { useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import {
  Check,
  CircleHelp,
  Eye,
  EyeOff,
  LogOut,
  Monitor,
  Moon,
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
} from "./api";
import { GatewaySwitcher } from "./GatewaySwitcher";
import { HelpDialog } from "./HelpDialog";
import { PrivacyProvider, storedHideEmails, storeHideEmails } from "./Privacy";
import { pages, Workspace, type Page } from "./Workspace";

const pageOf = (hash: string) =>
  pages.find((p) => p.toLowerCase() === hash.slice(1)) || "Overview";
const themes = ["system", "dark", "light"] as const;
type Theme = (typeof themes)[number];
const themeIcons = { system: Monitor, dark: Moon, light: Sun };
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
  const [gateways, setGateways] = useState<Gateway[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [error, setError] = useState("");
  const [loginError, setLoginError] = useState("");
  const [token, setToken] = useState("");
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
  const [help, setHelp] = useState(false);
  const light = theme === "system" ? systemLight : theme === "light";

  function signedOut(info: AuthInfo | null, message: string) {
    bearer.current = "";
    setAuth(info);
    setGateways([]);
    setSelected(null);
    if (message) setLoginError(message);
    setPhase("login");
  }
  // Checks management access, then loads the available account stores. `tried` is set
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
      if (info.mode === "token" && !bearer.current)
        return signedOut(info, rejected);
      const list = await client.request<{ gateways: Gateway[] }>(
        "/api/gateways",
      );
      if (current !== sequence.current) return;
      const next = list.gateways || [];
      setAuth(info);
      setGateways(next);
      setSelected(pickGateway(next, storedGateway()));
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

  const canSignOut = phase === "ready" && !!bearer.current;
  async function signOut() {
    bearer.current = "";
    setLoginError("");
    void load();
  }
  const sessionEnded = () => {
    if (ending.current) return;
    ending.current = true;
    if (auth?.mode === "external") {
      // A document navigation lets the authentication proxy start login again.
      location.reload();
      return;
    }
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
  const nextTheme = themes[(themes.indexOf(theme) + 1) % themes.length];
  const ThemeIcon = themeIcons[theme];

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
        {phase === "ready" && gateways.length > 1 && (
          <GatewaySwitcher
            gateways={gateways}
            selected={gateway ? gateway.id : null}
            onSelect={select}
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
            onClick={() => setHelp(true)}
          >
            <CircleHelp size={16} />
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
          <div className="loading">
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
                    autoComplete="current-password"
                  />
                  <button className="primary">Sign in with token</button>
                </form>
              </>
            )}
          </section>
        )}
        {phase === "ready" && !gateway && (
          <div className="empty">
            <p>No gateway is available.</p>
          </div>
        )}
        {phase === "ready" && gateway && auth && (
          <Workspace
            key={gateway.id}
            gateway={gateway}
            page={page}
            navigate={navigate}
            token={() => bearer.current}
            authMode={auth.mode}
            publicUrl={auth.publicUrl}
            onUnauthorized={sessionEnded}
            onGone={() => void reloadGateways()}
            copy={copy}
            notify={setToast}
          />
        )}
      </main>
      {help && (
        <HelpDialog
          publicUrl={auth?.publicUrl || ""}
          copy={copy}
          onClose={() => setHelp(false)}
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
