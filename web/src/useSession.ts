import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type RefObject,
} from "react";
import { createClient, errorMessage, type AuthInfo } from "./api";

// Management sign-in. "login" covers every signed-out state, "ready" every
// signed-in one.
export function useSession(
  // Set by the workspace while it has unsaved model drafts.
  unsaved: RefObject<boolean>,
) {
  // Sign-in only. The workspace has its own client, which closes on sign-out.
  const [client] = useState(() => createClient({}));
  const [phase, setPhase] = useState<"loading" | "login" | "ready" | "error">(
    "loading",
  );
  const [auth, setAuth] = useState<AuthInfo | null>(null);
  const [error, setError] = useState("");
  const [loginError, setLoginError] = useState("");
  const sequence = useRef(0);
  // Several requests can report the same expired session.
  const ending = useRef(false);

  // The session lives in a cookie this page cannot read, so a reload stays
  // signed in.
  const load = useCallback(async () => {
    const current = ++sequence.current;
    setPhase("loading");
    setError("");
    try {
      const info = await client.request<AuthInfo>("/api/auth");
      if (current !== sequence.current) return;
      setAuth(info);
      if (info.signedIn) {
        setLoginError("");
        ending.current = false;
      }
      setPhase(info.signedIn ? "ready" : "login");
    } catch (err) {
      if (current !== sequence.current) return;
      setError(errorMessage(err, "Could not load vrouter."));
      setPhase("error");
    }
  }, [client]);
  useEffect(() => {
    void load();
  }, [load]);

  // The key is sent once and never kept: the server answers with the cookie.
  async function signIn(key: string) {
    const current = ++sequence.current;
    setLoginError("");
    setPhase("loading");
    try {
      await client.request("/api/auth/session", "POST", { key });
    } catch (err) {
      if (current !== sequence.current) return;
      setLoginError(errorMessage(err, "Could not sign in."));
      setPhase("login");
      return;
    }
    if (current === sequence.current) void load();
  }
  async function signOut() {
    if (
      unsaved.current &&
      !confirm("Discard unsaved model changes and sign out?")
    )
      return;
    setLoginError("");
    try {
      await client.request("/api/auth/session", "DELETE");
    } catch {
      // The reload below shows whether the session is still open.
    }
    void load();
  }
  const sessionEnded = () => {
    if (ending.current) return;
    ending.current = true;
    setLoginError("Your session ended. Sign in again.");
    void load();
  };

  return {
    phase,
    auth,
    error,
    loginError,
    load,
    signIn,
    signOut,
    sessionEnded,
  };
}
