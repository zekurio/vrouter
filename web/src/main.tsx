import { useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { Check, RefreshCw, TriangleAlert } from "lucide-react";
import "./style.css";
import "@fontsource-variable/dm-sans";
import { createClient, type AuthInfo } from "./api";
import { HelpDialog } from "./HelpDialog";
import { PrivacyProvider, storedHideEmails, storeHideEmails } from "./Privacy";
import { readStored, writeStored } from "./storage";
import { TopBar, type Theme } from "./TopBar";
import { gatewayKey, useSession } from "./useSession";
import { pages, Workspace, type Page } from "./Workspace";

const pageOf = (hash: string) =>
  pages.find((p) => p.toLowerCase() === hash.slice(1)) ?? "Overview";
const themeKey = "vrouter-theme";
const storedTheme = (): Theme => {
  const value = readStored(themeKey);
  return value === "system" || value === "light" ? value : "dark";
};

// The chosen theme, following the system setting when it is "system".
function useTheme() {
  const [theme, setTheme] = useState<Theme>(storedTheme);
  const [systemLight, setSystemLight] = useState(
    () => matchMedia("(prefers-color-scheme: light)").matches,
  );
  const light = theme === "system" ? systemLight : theme === "light";
  useEffect(() => {
    const system = matchMedia("(prefers-color-scheme: light)");
    const scheme = () => setSystemLight(system.matches);
    system.addEventListener("change", scheme);
    return () => system.removeEventListener("change", scheme);
  }, []);
  useEffect(() => {
    document.documentElement.dataset["theme"] = light ? "light" : "dark";
    writeStored(themeKey, theme);
  }, [light, theme]);
  return { theme, setTheme };
}

type ToastMessage = { text: string; failed?: boolean };

// A short message that hides itself after a moment.
function useToast() {
  const [toast, setToast] = useState<ToastMessage | null>(null);
  useEffect(() => {
    if (!toast) return undefined;
    const id = setTimeout(() => setToast(null), 2500);
    return () => clearTimeout(id);
  }, [toast]);
  return [toast, setToast] as const;
}

function App() {
  const [page, setPage] = useState<Page>(() => pageOf(location.hash));
  // Not scoped to a gateway: sign-in and the gateway list work without one.
  const [client] = useState(() => createClient({}));
  // Set by the open gateway while it has unsaved model drafts.
  const unsaved = useRef(false);
  const session = useSession(client, unsaved);
  const { phase, auth, gateways, selected } = session;
  const [toast, setToast] = useToast();
  const { theme, setTheme } = useTheme();
  const [hideEmails, setHideEmails] = useState(storedHideEmails);
  const [help, setHelp] = useState(false);

  useEffect(() => {
    const change = () => setPage(pageOf(location.hash));
    window.addEventListener("hashchange", change);
    return () => window.removeEventListener("hashchange", change);
  }, []);
  useEffect(() => storeHideEmails(hideEmails), [hideEmails]);

  const select = (id: string) => {
    if (id === selected) return;
    if (
      unsaved.current &&
      !confirm("Discard unsaved model changes and switch gateways?")
    )
      return;
    writeStored(gatewayKey, id);
    session.setSelected(id);
  };
  const navigate = (next: Page) => {
    setPage(next);
    location.hash = next.toLowerCase();
  };
  const copy = async (value: string) => {
    try {
      await navigator.clipboard.writeText(value);
      setToast({ text: "Copied to clipboard" });
      return true;
    } catch {
      setToast({
        text: "Clipboard unavailable. Select and copy the text.",
        failed: true,
      });
      return false;
    }
  };
  const gateway = gateways.find((g) => g.id === selected);

  return (
    <PrivacyProvider value={hideEmails}>
      <TopBar
        gateway={gateway}
        gateways={gateways}
        ready={phase === "ready"}
        page={page}
        navigate={navigate}
        onSelect={select}
        actions={{
          hideEmails,
          setHideEmails,
          theme,
          setTheme,
          onHelp: () => setHelp(true),
          onSignOut: session.canSignOut
            ? () => void session.signOut()
            : undefined,
        }}
      />
      <main>
        {phase === "loading" && (
          <div className="loading" role="status" aria-label="Loading">
            <RefreshCw size={22} className="spinning" />
          </div>
        )}
        {phase === "error" && (
          <div className="notice error" role="alert">
            {session.error}{" "}
            <button onClick={() => void session.load()}>Try again</button>
          </div>
        )}
        {phase === "login" && auth && (
          <LoginPanel
            auth={auth}
            error={session.loginError}
            token={session.token}
            setToken={session.setToken}
            onRetry={() => void session.load()}
            onSignIn={() => void session.signIn()}
          />
        )}
        {phase === "ready" && !gateway && (
          <NoGateway others={gateways.length > 0} />
        )}
        {phase === "ready" && gateway && auth && (
          <Workspace
            key={gateway.id}
            gateway={gateway}
            page={page}
            navigate={navigate}
            publicUrl={auth.publicUrl}
            onUnauthorized={session.sessionEnded}
            onGone={() => void session.reloadGateways()}
            copy={copy}
            notify={(text) => setToast({ text })}
            onDirty={(dirty) => {
              unsaved.current = dirty;
            }}
          />
        )}
      </main>
      {help && (
        <HelpDialog
          publicUrl={auth?.publicUrl ?? ""}
          copy={copy}
          onClose={() => setHelp(false)}
        />
      )}
      {toast && <Toast toast={toast} />}
    </PrivacyProvider>
  );
}

function NoGateway({ others }: { others: boolean }) {
  return (
    <div className="empty">
      <p>
        {others
          ? "That gateway is no longer available. Choose another."
          : "No gateway is available."}
      </p>
    </div>
  );
}

function Toast({ toast }: { toast: ToastMessage }) {
  return (
    <div className="toast" role={toast.failed === true ? "alert" : "status"}>
      {toast.failed === true ? (
        <TriangleAlert size={15} />
      ) : (
        <Check size={15} />
      )}
      {toast.text}
    </div>
  );
}

function LoginPanel({
  auth,
  error,
  token,
  setToken,
  onRetry,
  onSignIn,
}: {
  auth: AuthInfo;
  error: string;
  token: string;
  setToken: (token: string) => void;
  onRetry: () => void;
  onSignIn: () => void;
}) {
  return (
    <section className="login-panel">
      <h1>Sign in</h1>
      {error && (
        <div className="notice error" role="alert">
          {error}
        </div>
      )}
      {auth.mode === "local" ? (
        <>
          <p className="login-note">
            This vrouter has no sign-in configured. It only answers a browser
            running on the same machine, at a loopback address.
          </p>
          <button className="primary" onClick={onRetry}>
            Try again
          </button>
        </>
      ) : auth.mode === "external" ? (
        <>
          <p className="login-note">
            Your sign-in proxy did not accept this session.
          </p>
          {/* A document navigation lets the proxy start login again. */}
          <button className="primary" onClick={() => location.reload()}>
            Sign in again
          </button>
        </>
      ) : (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            onSignIn();
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
      )}
    </section>
  );
}

createRoot(document.querySelector("#root")!).render(<App />);
