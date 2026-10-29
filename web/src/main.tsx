import { useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { Check, LogIn, RefreshCw, TriangleAlert } from "lucide-react";
import "./style.css";
import "@fontsource-variable/inter/opsz.css";
import { createClient, type AuthInfo } from "./api";
import { HelpDialog } from "./HelpDialog";
import { PrivacyProvider, storedHideEmails, storeHideEmails } from "./Privacy";
import { TopBar } from "./TopBar";
import { button, cx, loading, notice, noticeAction } from "./ui";
import { useSession } from "./useSession";
import { pages, pagesFor, Workspace, type Page } from "./Workspace";

const pageOf = (hash: string) =>
  pages.find((p) => p.toLowerCase() === hash.slice(1)) ?? "Overview";
// The page column. Signed in, a phone pins the navigation to the bottom
// edge, so the column leaves room for it.
const mainClass = (nav: boolean) =>
  cx(
    "mx-auto mt-5.5 max-w-[1440px] px-7.5 pb-16.25 max-xl:px-6 max-xl:pb-15 max-sm:mt-3 max-sm:px-4 max-sm:pb-12.5",
    nav && "max-sm:pb-[113px]",
  );

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
          onHelp: () => setHelp(true),
          onSignOut: () => void session.signOut(),
        }}
      />
      <main className={mainClass(phase === "ready")}>
        {phase === "loading" && (
          <div className={loading} role="status" aria-label="Loading">
            <RefreshCw size={22} className="animate-spin" />
          </div>
        )}
        {phase === "error" && (
          <div className={cx(notice({ error: true }), "my-3.75")} role="alert">
            {session.error}{" "}
            <button
              className={noticeAction}
              onClick={() => void session.load()}
            >
              Try again
            </button>
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
      {toast && <Toast toast={toast} aboveNav={phase === "ready"} />}
    </PrivacyProvider>
  );
}

function Toast({
  toast,
  aboveNav,
}: {
  toast: ToastMessage;
  // Lifts it over the navigation bar pinned to the bottom of a phone.
  aboveNav: boolean;
}) {
  return (
    <div
      className={cx(
        "fixed bottom-6.25 left-1/2 z-30 flex w-max max-w-[90vw] -translate-x-1/2 items-center gap-2.25 rounded-[10px] bg-ink px-4 py-2.75 text-[13px] font-medium text-on-ink shadow-[0_6px_30px_#0003]",
        aboveNav && "max-sm:bottom-19",
      )}
      role={toast.failed === true ? "alert" : "status"}
    >
      {toast.failed === true ? (
        <TriangleAlert size={15} className="shrink-0" />
      ) : (
        <Check size={15} className="shrink-0" />
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
    <section className="mx-auto mt-20 mb-37.5 max-w-[380px] rounded-2xl bg-surface p-7.5 light:ring-1 light:ring-border max-sm:mt-12.5 max-sm:mb-22.5">
      <h1 className="text-[26px] leading-[1.2] font-semibold tracking-[-0.6px]">
        Sign in
      </h1>
      <p className="my-4.5 text-[13px] leading-[1.6] text-subtle">
        vrouter signs you in through{" "}
        <strong className="font-semibold text-text wrap-anywhere">
          {auth.provider}
        </strong>
        . Your API keys and usage are your own; admins also manage shared
        accounts and users.
      </p>
      <button
        className={cx(button("primary", "wide"), "mt-1.5")}
        onClick={onSignIn}
      >
        <LogIn size={15} /> Sign in with {auth.provider}
      </button>
    </section>
  );
}

createRoot(document.querySelector("#root")!).render(<App />);
