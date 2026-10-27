import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type RefObject,
} from "react";
import {
  errorMessage,
  statusOf,
  type AuthInfo,
  type createClient,
  type SessionUser,
} from "./api";

// Why the server sent the browser back from a sign-in, keyed by ?signin=.
const signInErrors: Record<string, string> = {
  unavailable:
    "vrouter could not reach the identity provider. Try again in a moment.",
  expired:
    "The sign-in expired or was opened in another browser. Start it again.",
  cancelled: "Sign-in was cancelled.",
  failed:
    "Sign-in failed. Try again, or ask an admin to check the vrouter log.",
  denied:
    "Your account is not in a group that may use vrouter. Ask an admin for access.",
  disabled: "Your vrouter access is disabled. Ask an admin to enable it.",
};

// Reads and removes the reason a sign-in came back without a session.
function returnedSignInError() {
  const params = new URLSearchParams(location.search);
  const reason = params.get("signin");
  if (reason === null) return "";
  params.delete("signin");
  const query = params.toString();
  history.replaceState(
    null,
    "",
    `${location.pathname}${query ? `?${query}` : ""}${location.hash}`,
  );
  return signInErrors[reason] ?? signInErrors["failed"] ?? "";
}

// The server sends the browser to the identity provider and back.
const signIn = () => location.assign("/api/auth/login");

// Management sign-in. "login" covers every signed-out state, "ready" every
// signed-in one.
export function useSession(
  client: ReturnType<typeof createClient>,
  // Set by the workspace while it has unsaved model drafts.
  unsaved: RefObject<boolean>,
) {
  const [phase, setPhase] = useState<"loading" | "login" | "ready" | "error">(
    "loading",
  );
  const [auth, setAuth] = useState<AuthInfo | null>(null);
  const [user, setUser] = useState<SessionUser | null>(null);
  const [error, setError] = useState("");
  const [loginError, setLoginError] = useState(returnedSignInError);
  const sequence = useRef(0);
  // Several requests can report the same expired session.
  const ending = useRef(false);

  // The session lives in a cookie this page cannot read, so a reload stays
  // signed in.
  const load = useCallback(async () => {
    const current = ++sequence.current;
    setPhase("loading");
    setError("");
    let info: AuthInfo | null = null;
    try {
      info = await client.request<AuthInfo>("/api/auth");
      if (current !== sequence.current) return;
      const session = await client.request<{ user: SessionUser }>(
        "/api/auth/session",
      );
      if (current !== sequence.current) return;
      setAuth(info);
      setUser(session.user);
      setLoginError("");
      ending.current = false;
      setPhase("ready");
    } catch (err) {
      if (current !== sequence.current) return;
      if (info && statusOf(err) === 401) {
        setAuth(info);
        setUser(null);
        setPhase("login");
        return;
      }
      setError(errorMessage(err, "Could not load vrouter."));
      setPhase("error");
    }
  }, [client]);
  useEffect(() => {
    void load();
  }, [load]);

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
    user,
    error,
    loginError,
    load,
    signIn,
    signOut,
    sessionEnded,
  };
}
