import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type RefObject,
} from "react";
import {
  errorMessage,
  pickGateway,
  statusOf,
  type AuthInfo,
  type Gateway,
  type createClient,
} from "./api";
import { readStored } from "./storage";

export const gatewayKey = "vrouter-gateway";

// Management sign-in and the gateways it opens. "login" covers every
// signed-out state, "ready" every signed-in one.
export function useSession(
  client: ReturnType<typeof createClient>,
  // Set by the open gateway while it has unsaved model drafts.
  unsaved: RefObject<boolean>,
) {
  const [phase, setPhase] = useState<"loading" | "login" | "ready" | "error">(
    "loading",
  );
  const [auth, setAuth] = useState<AuthInfo | null>(null);
  const [gateways, setGateways] = useState<Gateway[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [error, setError] = useState("");
  const [loginError, setLoginError] = useState("");
  const [token, setToken] = useState("");
  const sequence = useRef(0);
  // Several requests can report the same expired session.
  const ending = useRef(false);

  // Checks management access, then loads the available account stores. With an
  // admin token the server keeps the sign-in in a cookie this page cannot read,
  // so a reload stays signed in.
  const load = useCallback(async () => {
    const current = ++sequence.current;
    setPhase("loading");
    setError("");
    let info: AuthInfo | null = null;
    try {
      info = await client.request<AuthInfo>("/api/auth");
      if (current !== sequence.current) return;
      const list = await client.request<{ gateways: Gateway[] | null }>(
        "/api/gateways",
      );
      if (current !== sequence.current) return;
      const next = list.gateways ?? [];
      setAuth(info);
      setGateways(next);
      setSelected(pickGateway(next, readStored(gatewayKey)));
      setLoginError("");
      ending.current = false;
      setPhase("ready");
    } catch (err) {
      if (current !== sequence.current) return;
      if (info && statusOf(err) === 401) {
        setAuth(info);
        setGateways([]);
        setSelected(null);
        setPhase("login");
        return;
      }
      setError(errorMessage(err, "Could not load vrouter."));
      setPhase("error");
    }
  }, [client]);
  // Keeps the open gateway unless the server stopped listing it.
  async function reloadGateways() {
    const current = sequence.current;
    try {
      const list = await client.request<{ gateways: Gateway[] | null }>(
        "/api/gateways",
      );
      if (current !== sequence.current) return;
      const next = list.gateways ?? [];
      setGateways(next);
      setSelected((id) =>
        id !== null && id !== "" && next.some((g) => g.id === id) ? id : null,
      );
    } catch {
      // The open gateway already shows its own error.
    }
  }
  useEffect(() => {
    void load();
  }, [load]);

  async function signIn() {
    const current = ++sequence.current;
    const value = token;
    setToken("");
    setLoginError("");
    setPhase("loading");
    try {
      await client.request("/api/auth/session", "POST", { token: value });
    } catch (err) {
      if (current !== sequence.current) return;
      setLoginError(errorMessage(err, "Could not sign in."));
      setPhase("login");
      return;
    }
    if (current === sequence.current) void load();
  }
  const canSignOut = phase === "ready" && auth?.mode === "token";
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
    if (auth?.mode === "external") {
      // A document navigation lets the authentication proxy start login again.
      location.reload();
      return;
    }
    setLoginError("Your session ended. Sign in again.");
    void load();
  };

  return {
    phase,
    auth,
    gateways,
    selected,
    setSelected,
    error,
    loginError,
    token,
    setToken,
    load,
    reloadGateways,
    signIn,
    canSignOut,
    signOut,
    sessionEnded,
  };
}
