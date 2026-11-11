import { useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import "./style.css";
import "@fontsource-variable/inter/opsz.css";
import { createClient } from "./api";
import { HelpDialog } from "./HelpDialog";
import { PrivacyProvider, storedHideEmails, storeHideEmails } from "./Privacy";
import { SignedOut } from "./SignedOut";
import { ToastProvider, useToast } from "./Toaster";
import { TopBar } from "./TopBar";
import { useSession } from "./useSession";
import { pages, pagesFor, Workspace, type Page } from "./Workspace";

const pageOf = (hash: string) =>
  pages.find((p) => p.toLowerCase() === hash.slice(1)) ?? "Overview";
// The page column. Signed in, a phone pins the navigation to the bottom
// edge, so the column leaves room for it.
const mainColumn =
  "mx-auto mt-5.5 max-w-[1280px] px-7.5 pb-16.25 max-xl:px-6 max-xl:pb-15 max-sm:mt-3 max-sm:px-4";
const mainClass = (nav: boolean) =>
  `${mainColumn} ${nav ? "max-sm:pb-[calc(113px+env(safe-area-inset-bottom))]" : "max-sm:pb-12.5"}`;

function App() {
  const [page, setPage] = useState<Page>(() => pageOf(location.hash));
  // Sign-in only. The workspace has its own client, which closes on sign-out.
  const [client] = useState(() => createClient({}));
  // Set by the workspace while it has unsaved model drafts.
  const unsaved = useRef(false);
  const session = useSession(client, unsaved);
  const { phase, auth, user } = session;
  // A member who follows a link to an admin page sees the overview.
  const shown = user && pagesFor(user.role).includes(page) ? page : "Overview";
  const toast = useToast();
  const [hideEmails, setHideEmails] = useState(storedHideEmails);
  const [help, setHelp] = useState(false);

  useEffect(() => {
    const change = () => setPage(pageOf(location.hash));
    window.addEventListener("hashchange", change);
    return () => window.removeEventListener("hashchange", change);
  }, []);
  useEffect(() => storeHideEmails(hideEmails), [hideEmails]);

  const navigate = (next: Page) => {
    setPage(next);
    location.hash = next.toLowerCase();
  };
  const copy = async (value: string) => {
    try {
      await navigator.clipboard.writeText(value);
      toast("Copied to clipboard");
      return true;
    } catch {
      toast("Clipboard unavailable. Select and copy the text.", {
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
        {phase !== "ready" && <SignedOut session={session} />}
        {phase === "ready" && auth && user && (
          <Workspace
            page={shown}
            user={user}
            navigate={navigate}
            publicUrl={auth.publicUrl}
            onUnauthorized={session.sessionEnded}
            copy={copy}
            notify={toast}
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
    </PrivacyProvider>
  );
}

createRoot(document.querySelector("#root")!).render(
  <ToastProvider>
    <App />
  </ToastProvider>,
);
