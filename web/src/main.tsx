import { useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import "./style.css";
import "@fontsource-variable/inter/opsz.css";
import { HelpDialog } from "./HelpDialog";
import { PrivacyProvider, storedHideEmails, storeHideEmails } from "./Privacy";
import { SignedOut } from "./SignedOut";
import { ToastProvider, useToast } from "./Toaster";
import { TopBar } from "./TopBar";
import { useSession } from "./useSession";
import { usePage } from "./router";
import { Workspace } from "./Workspace";

// The page column. Signed in, a phone pins the navigation to the bottom
// edge, so the column leaves room for it.
const mainColumn =
  "mx-auto mt-5.5 max-w-[1280px] px-7.5 pb-16.25 max-xl:px-6 max-xl:pb-15 max-sm:mt-3 max-sm:px-4";
const mainClass = (nav: boolean) =>
  `${mainColumn} ${nav ? "max-sm:pb-[calc(113px+env(safe-area-inset-bottom))]" : "max-sm:pb-12.5"}`;

function App() {
  const [page, navigate] = usePage();
  // Set by the workspace while it has unsaved model drafts.
  const unsaved = useRef(false);
  const session = useSession(unsaved);
  const { phase, auth } = session;
  const toast = useToast();
  const [hideEmails, setHideEmails] = useState(storedHideEmails);
  const [help, setHelp] = useState(false);

  useEffect(() => storeHideEmails(hideEmails), [hideEmails]);

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
        signedIn={phase === "ready"}
        page={page}
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
        {phase === "ready" && auth && (
          <Workspace
            page={page}
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
