import { useState } from "react";
import { LogIn, RefreshCw } from "lucide-react";
import {
  button,
  cx,
  fieldLabel,
  input,
  loading,
  notice,
  noticeAction,
} from "./ui";
import type { useSession } from "./useSession";

// Everything before the workspace: the first load, a vrouter that does not
// answer, and sign-in.
export function SignedOut({
  session,
}: {
  session: ReturnType<typeof useSession>;
}) {
  const { phase } = session;
  const [key, setKey] = useState("");
  if (phase === "loading")
    return (
      <div className={loading} role="status" aria-label="Loading">
        <RefreshCw size={22} className="animate-spin" />
      </div>
    );
  if (phase === "error")
    return (
      <div className={cx(notice({ error: true }), "my-3.75")} role="alert">
        {session.error}{" "}
        <button className={noticeAction} onClick={() => void session.load()}>
          Try again
        </button>
      </div>
    );
  if (phase !== "login") return null;
  return (
    // Centred in the space under the header, a little above the middle.
    <div className="flex min-h-[calc(100dvh-190px)] items-center justify-center pb-[8dvh]">
      <section className="w-full max-w-[380px] rounded-2xl bg-surface p-7.5 light:ring-1 light:ring-border max-sm:p-6">
        <h1 className="text-[26px] leading-[1.2] font-semibold tracking-[-0.6px]">
          Sign in to vrouter
        </h1>
        <p className="mt-3 text-[14px] leading-[1.55] text-text">
          One endpoint for your Claude and Codex accounts.
        </p>
        {/* Why the last sign-in or session ended. */}
        {session.loginError && (
          <div
            className={cx(notice({ error: true, inset: true }), "mt-4.5")}
            role="alert"
          >
            {session.loginError}
          </div>
        )}
        <form
          className="mt-5.5"
          onSubmit={(e) => {
            e.preventDefault();
            void session.signIn(key);
          }}
        >
          <label className={fieldLabel} htmlFor="admin-key">
            Admin key
          </label>
          <input
            id="admin-key"
            className={input}
            type="password"
            value={key}
            onChange={(e) => setKey(e.target.value)}
            required
            // oxlint-disable-next-line jsx-a11y/no-autofocus -- the page's only field
            autoFocus
            autoComplete="current-password"
          />
          <button className={cx(button("primary", "wide"), "mt-4")}>
            <LogIn size={15} /> Sign in
          </button>
        </form>
        <p className="mt-4.5 text-[12px] leading-[1.6] text-muted">
          The key is <code>VROUTER_ADMIN_KEY</code>. Without one, vrouter prints
          a new key each time it starts.
        </p>
      </section>
    </div>
  );
}
