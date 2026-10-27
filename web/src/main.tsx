import { useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { Check, LogIn, RefreshCw, TriangleAlert } from "lucide-react";
import "./style.css";
import "@fontsource-variable/inter/opsz.css";
import { createClient, type AuthInfo } from "./api";
import { HelpDialog } from "./HelpDialog";
import { PrivacyProvider, storedHideEmails, storeHideEmails } from "./Privacy";
import { readStored, writeStored } from "./storage";
import { TopBar, type Theme } from "./TopBar";
import { useSession } from "./useSession";
import { pages, pagesFor, Workspace, type Page } from "./Workspace";

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

// A short message that hides itself after a moment. Failures stay longer, so
// there is time to read them.
function useToast() {
  const [toast, setToast] = useState<ToastMessage | null>(null);
  useEffect(() => {
    if (!toast) return undefined;
    const id = setTimeout(
      () => setToast(null),
      toast.failed === true ? 6000 : 2500,
    );
    return () => clearTimeout(id);
  }, [toast]);
  return [toast, setToast] as const;
}

function App() {
  const [page, setPage] = useState<Page>(() => pageOf(location.hash));
  // Sign-in only. The workspace has its own client, which closes on sign-out.
  const [client] = useState(() => createClient({}));
  // Set by the workspace while it has unsaved model drafts.
  const unsaved = useRef(false);
  const session = useSession(client, unsaved);
  const { phase, auth, user, loginError } = session;
  // A member who follows a link to an admin page sees the overview.
  const shown = user && pagesFor(user.role).includes(page) ? page : "Overview";
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
  useEffect(() => {
    if (loginError) setToast({ text: loginError, failed: true });
  }, [loginError, setToast]);

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

  return (
    <PrivacyProvider value={hideEmails}>
      <TopBar
        user={phase === "ready" ? user : null}
        page={shown}
        navigate={navigate}
        actions={{
          hideEmails,
          setHideEmails,
          theme,
          setTheme,
          onHelp: () => setHelp(true),
          onSignOut: () => void session.signOut(),
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
          <LoginPanel auth={auth} onSignIn={session.signIn} />
        )}
        {phase === "ready" && auth && user && (
          <Workspace
            page={shown}
            user={user}
            navigate={navigate}
            publicUrl={auth.publicUrl}
            onUnauthorized={session.sessionEnded}
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
  onSignIn,
}: {
  auth: AuthInfo;
  onSignIn: () => void;
}) {
  return (
    <section className="login-panel">
      <h1>Sign in</h1>
      <p className="login-note">
        vrouter signs you in through{" "}
        <strong className="login-issuer">{auth.provider}</strong>. Your API keys
        and usage are your own; admins also manage shared accounts and users.
      </p>
      <button className="primary login-button" onClick={onSignIn}>
        <LogIn size={15} /> Sign in with {auth.provider}
      </button>
    </section>
  );
}

createRoot(document.querySelector("#root")!).render(<App />);
