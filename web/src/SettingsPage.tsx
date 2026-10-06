import type { ReactNode } from "react";
import { Copy, LogOut } from "lucide-react";
import type { AuthMode, Gateway, User } from "./api";
import { Private } from "./Privacy";

export type Engine = {
  version?: string;
  catalog: "ok" | "missing" | "error";
  management: "ok" | "missing" | "error";
  adminToken: boolean;
  clientKey: boolean;
  storage: "local";
};
export type Theme = "system" | "dark" | "light";
type Props = {
  mode: "demo" | "live" | "unconfigured";
  engine: Engine;
  models: number;
  accounts: number;
  endpoint: string;
  copy: (value: string) => void;
  theme: Theme;
  setTheme: (theme: Theme) => void;
  gateway: Gateway;
  user: User | null;
  authMode: AuthMode;
  canSignOut: boolean;
  onSignOut: () => void;
};
type Check = {
  label: string;
  variable: string;
  state: "ok" | "missing" | "error" | "idle";
  value: ReactNode;
};
const count = (n: number, noun: string) => `${n} ${noun}${n === 1 ? "" : "s"}`;

export function SettingsPage({
  mode,
  engine,
  models,
  accounts,
  endpoint,
  copy,
  theme,
  setTheme,
  gateway,
  user,
  authMode,
  canSignOut,
  onSignOut,
}: Props) {
  const live = mode === "live";
  const owner = user?.id === gateway.ownerId;
  // The built-in administrator signs in with the admin token, not a cookie.
  const tokenSession = authMode !== "oidc" || user?.id === "local-admin";
  const example = `curl ${endpoint}/models \\\n  -H "Authorization: Bearer $VROUTER_API_KEY"`;
  const checks: Check[] = [
    {
      label: "Private storage",
      variable: "VROUTER_DATA_DIR",
      state: !live ? "idle" : engine.management === "ok" ? "ok" : "error",
      value: !live ? (
        "Not used in demo mode"
      ) : engine.management === "ok" ? (
        <>
          Local store loaded
          {engine.version && <span>{engine.version} engine</span>}
        </>
      ) : (
        "Could not read the local store. Check that the directory is writable."
      ),
    },
    {
      label: "Server API key",
      variable: "VROUTER_API_KEY",
      state: live && engine.clientKey ? "ok" : "idle",
      value: !live ? (
        "Not checked"
      ) : engine.clientKey ? (
        "Set. It opens the original gateway only."
      ) : (
        <a className="text-link" href="#keys">
          Not set. Clients need a key from API keys.
        </a>
      ),
    },
    {
      label: "Model catalog",
      variable: "/v1/models",
      state: live ? engine.catalog : "idle",
      value: !live
        ? "Not checked"
        : engine.catalog === "ok"
          ? count(models, "model")
          : engine.catalog === "missing"
            ? "No models yet. Connect an account to fill the catalog."
            : "Could not build the catalog from the stored accounts.",
    },
    {
      label: "Connected accounts",
      variable: "/api/accounts",
      state: live ? engine.management : "idle",
      value: !live ? (
        "Not checked"
      ) : engine.management === "ok" ? (
        accounts === 0 ? (
          <a className="text-link" href="#accounts">
            None yet. Add an account.
          </a>
        ) : (
          <a className="text-link" href="#accounts">
            {count(accounts, "account")}
          </a>
        )
      ) : (
        "Accounts are unavailable until the local store loads."
      ),
    },
    {
      label: "Admin access",
      variable: "VROUTER_ADMIN_TOKEN",
      state: live ? "ok" : "idle",
      value: !live
        ? "Not checked"
        : authMode === "oidc"
          ? engine.adminToken
            ? "Single sign-on. The admin token also opens every gateway."
            : "Single sign-on"
          : engine.adminToken
            ? "Token required to open this workspace"
            : "No token. Only connections from this machine are accepted.",
    },
  ];
  return (
    <div className="settings">
      <section aria-labelledby="settings-client">
        <header>
          <h2 id="settings-client">Client API</h2>
          <p>
            Clients send one of this gateway's <a href="#keys">API keys</a> to
            this address. The address is the same for every gateway, and the key
            decides which one answers. Codex models use{" "}
            <code>/v1/responses</code>, Claude models use{" "}
            <code>/v1/messages</code>. vrouter does not translate between them.
          </p>
        </header>
        <div>
          <div className="copy-field">
            <code>{endpoint}</code>
            <button
              className="icon-button"
              aria-label="Copy base URL"
              onClick={() => copy(endpoint)}
            >
              <Copy size={14} />
            </button>
          </div>
          <div className="copy-field block">
            <pre>{example}</pre>
            <button
              className="icon-button"
              aria-label="Copy example request"
              onClick={() => copy(example)}
            >
              <Copy size={14} />
            </button>
          </div>
          {!live && (
            <p className="settings-note">
              Demo data. Requests are not forwarded.
            </p>
          )}
        </div>
      </section>
      <section aria-labelledby="settings-engine">
        <header>
          <h2 id="settings-engine">Gateway</h2>
          <p>
            vrouter reads these from its environment at startup. Restart it
            after changing one. Provider tokens stay on the server and never
            reach this page.
          </p>
        </header>
        <div>
          {mode === "demo" && (
            <p className="settings-note">
              Demo mode is on, so the local store is not read. Unset{" "}
              <code>VROUTER_DEMO</code> and restart to load live data.
            </p>
          )}
          <dl className="engine-checks">
            {checks.map((check) => (
              <div key={check.variable}>
                <dt>
                  <span
                    className={`check-state ${check.state}`}
                    aria-hidden="true"
                  />
                  {check.label}
                  <code>{check.variable}</code>
                </dt>
                <dd>{check.value}</dd>
              </div>
            ))}
          </dl>
        </div>
      </section>
      <section aria-labelledby="settings-appearance">
        <header>
          <h2 id="settings-appearance">Appearance</h2>
          <p>Saved in this browser.</p>
        </header>
        <div>
          <div className="tabs" role="group" aria-label="Theme">
            {(["system", "dark", "light"] as Theme[]).map((option) => (
              <button
                key={option}
                aria-pressed={theme === option}
                className={theme === option ? "selected" : ""}
                onClick={() => setTheme(option)}
              >
                {option[0].toUpperCase() + option.slice(1)}
              </button>
            ))}
          </div>
        </div>
      </section>
      <section aria-labelledby="settings-session">
        <header>
          <h2 id="settings-session">Session</h2>
          <p>
            {user && owner
              ? "You own this gateway."
              : user?.role === "admin"
                ? "Another user owns this gateway. You can manage it as an admin."
                : "Access to this gateway."}
          </p>
        </header>
        <div>
          <dl className="engine-checks">
            <div>
              <dt>Gateway</dt>
              <dd>
                {gateway.name}
                <code>{gateway.id}</code>
              </dd>
            </div>
            {user && (
              <div>
                <dt>Signed in as</dt>
                <dd>
                  <Private>{user.name}</Private>
                  {user.email && user.email !== user.name && (
                    <Private>{user.email}</Private>
                  )}
                  <span>{user.role === "admin" ? "Admin" : "User"}</span>
                </dd>
              </div>
            )}
          </dl>
          {canSignOut && (
            <>
              <p className="settings-note session-note">
                {tokenSession
                  ? "The admin token is held in memory only. Reloading the page signs you out."
                  : "Signing out ends this browser's session. API keys keep working."}
              </p>
              <button className="secondary" onClick={onSignOut}>
                <LogOut size={14} /> Sign out
              </button>
            </>
          )}
        </div>
      </section>
    </div>
  );
}
