import { useCallback, useEffect, useRef, useState } from "react";
import {
  Ban,
  Check,
  Copy,
  Pencil,
  Plus,
  RefreshCw,
  Trash2,
} from "lucide-react";
import { errorMessage, isStale, type APIRequest } from "./api";
import { Modal } from "./Modal";
import {
  keyState,
  limitText,
  parseLimit,
  stateLabel,
  usedShare,
  type APIKey,
} from "./keys";

type Props = {
  request: APIRequest;
  live: boolean;
  demo: boolean;
  reloadKey: string;
  endpoint: string;
  copy: (value: string) => Promise<boolean>;
  notify: (text: string) => void;
};
type Confirm = { action: "revoke" | "delete"; key: APIKey };

const number = (n: number) => n.toLocaleString();
const day = (value: string) =>
  new Date(value).toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });

export function KeysPage({
  request,
  live,
  demo,
  reloadKey,
  endpoint,
  copy,
  notify,
}: Props) {
  const [keys, setKeys] = useState<APIKey[] | null>(null);
  const [loading, setLoading] = useState(live);
  const [error, setError] = useState("");
  // null is closed, "new" creates, otherwise the key being edited.
  const [editing, setEditing] = useState<APIKey | "new" | null>(null);
  const [confirm, setConfirm] = useState<Confirm | null>(null);
  const [busy, setBusy] = useState(false);
  const [confirmError, setConfirmError] = useState("");
  const sequence = useRef(0);

  const load = useCallback(async () => {
    const current = ++sequence.current;
    setLoading(true);
    try {
      const result = await request<{ keys: APIKey[] }>("/api/keys");
      if (current !== sequence.current) return;
      setKeys(result.keys || []);
      setError("");
    } catch (err) {
      if (current !== sequence.current || isStale(err)) return;
      setError(errorMessage(err, "Could not load API keys."));
    } finally {
      if (current === sequence.current) setLoading(false);
    }
  }, [request]);
  useEffect(() => {
    if (live) void load();
  }, [live, reloadKey, load]);

  const put = (key: APIKey) =>
    setKeys((list) =>
      list?.some((k) => k.id === key.id)
        ? list.map((k) => (k.id === key.id ? key : k))
        : [key, ...(list || [])],
    );

  async function runConfirm() {
    if (!confirm) return;
    const { action, key } = confirm;
    setBusy(true);
    setConfirmError("");
    try {
      if (action === "revoke") {
        const result = await request<{ key: APIKey }>(
          `/api/keys/${encodeURIComponent(key.id)}`,
          "PATCH",
          { revoked: true },
        );
        put(result.key);
        notify("Key revoked");
      } else {
        await request(`/api/keys/${encodeURIComponent(key.id)}`, "DELETE");
        setKeys((list) => list?.filter((k) => k.id !== key.id) ?? null);
        notify("Key deleted");
      }
      setConfirm(null);
    } catch (err) {
      setConfirmError(
        errorMessage(
          err,
          action === "revoke"
            ? "Could not revoke the key."
            : "Could not delete the key.",
        ),
      );
    } finally {
      setBusy(false);
    }
  }
  const closeConfirm = () => {
    if (busy) return;
    setConfirm(null);
    setConfirmError("");
  };

  return (
    <div className="keys">
      <div className="keys-intro">
        <p>
          A key lets its holder call this gateway's models at{" "}
          <code>{endpoint}</code>. Every gateway shares that address. The key
          decides which gateway answers. Keys can't open this dashboard.
        </p>
        <button
          className="secondary"
          disabled={!live}
          onClick={() => setEditing("new")}
        >
          <Plus size={14} /> Create key
        </button>
      </div>
      {!live && !demo && (
        <div className="notice">
          API keys are unavailable until the gateway's local store loads.
        </div>
      )}
      {error && (
        <div className="notice error" role="alert">
          {error} <button onClick={() => void load()}>Try again</button>
        </div>
      )}
      {live && loading && !keys && !error && (
        <div className="empty">
          <RefreshCw size={22} className="spinning" />
        </div>
      )}
      {live && keys?.length === 0 && (
        <div className="empty">
          <h2>No API keys yet</h2>
          <p>
            Create one for each person or tool that should use this gateway.
          </p>
          <button className="primary" onClick={() => setEditing("new")}>
            <Plus size={14} /> Create key
          </button>
        </div>
      )}
      {live && !!keys?.length && (
        <ul className="key-list">
          {keys.map((key) => {
            const state = keyState(key);
            const dead = state === "revoked";
            return (
              <li className={`key-row ${dead ? "is-off" : ""}`} key={key.id}>
                <div className="account-identity">
                  <h3>{key.name || "Unnamed key"}</h3>
                  <p>
                    <code>{key.prefix}…</code>
                  </p>
                  <p>
                    {key.revokedAt
                      ? `Revoked ${day(key.revokedAt)}`
                      : `Created ${day(key.createdAt)}`}
                  </p>
                </div>
                <div className="account-windows">
                  <Meter
                    label="Requests"
                    used={key.usedRequests}
                    limit={key.limitRequests}
                  />
                  <Meter
                    label="Tokens"
                    used={key.usedTokens}
                    limit={key.limitTokens}
                  />
                </div>
                <div className="account-row-actions">
                  <button
                    className="icon-button"
                    aria-label={`Edit ${key.name}`}
                    title="Edit name and limits"
                    disabled={dead}
                    onClick={() => setEditing(key)}
                  >
                    <Pencil size={14} />
                  </button>
                  <button
                    className="icon-button is-danger"
                    aria-label={`Revoke ${key.name}`}
                    title="Revoke"
                    disabled={dead}
                    onClick={() => setConfirm({ action: "revoke", key })}
                  >
                    <Ban size={14} />
                  </button>
                  <button
                    className="icon-button is-danger"
                    aria-label={`Delete ${key.name}`}
                    title="Delete"
                    onClick={() => setConfirm({ action: "delete", key })}
                  >
                    <Trash2 size={15} />
                  </button>
                </div>
                {state !== "active" && (
                  <p className={`key-state ${state}`}>
                    <strong>{stateLabel[state]}.</strong>{" "}
                    {state === "revoked" &&
                      "Calls with this key are refused. Delete it to drop it from this list."}
                    {state === "uncertain" &&
                      "A call ended without a usage report, so vrouter can't tell how many tokens this key has used and refuses new calls. Change the token limit to unblock it, or create a replacement key."}
                    {state === "requests-spent" &&
                      "New calls are refused. Raise or clear the request limit to use this key again."}
                    {state === "tokens-spent" &&
                      "New calls are refused. Raise or clear the token limit to use this key again."}
                  </p>
                )}
              </li>
            );
          })}
        </ul>
      )}
      {live && !!keys?.length && (
        <p className="keys-footnote">
          Usage counts are lifetime totals and never reset. Token counts come
          from what the provider reports after each response, so a key can end
          above its token limit.
        </p>
      )}
      {editing && (
        <KeyDialog
          target={editing === "new" ? null : editing}
          request={request}
          copy={copy}
          onSaved={(key, created) => {
            put(key);
            if (!created) notify("Key updated");
          }}
          onClose={() => setEditing(null)}
        />
      )}
      {confirm && (
        <Modal
          className="confirm-dialog"
          labelledBy="key-confirm-title"
          locked={busy}
          onClose={closeConfirm}
        >
          <h2 id="key-confirm-title">
            {confirm.action === "revoke" ? "Revoke" : "Delete"}{" "}
            {confirm.key.name || "this key"}?
          </h2>
          <p>
            {confirm.action === "revoke"
              ? "vrouter refuses every new call made with this key. A response that is already running finishes. You can't undo this. To restore access, create a new key."
              : "vrouter refuses every new call made with this key and removes it from this list. A response that is already running finishes. Its past requests stay in the request log. You can't undo this."}
          </p>
          {confirmError && (
            <div className="notice error" role="alert">
              {confirmError}
            </div>
          )}
          <div className="dialog-actions">
            <button
              className="secondary"
              disabled={busy}
              onClick={closeConfirm}
            >
              Keep key
            </button>
            <button
              className="primary danger"
              disabled={busy}
              onClick={() => void runConfirm()}
            >
              {confirm.action === "revoke"
                ? busy
                  ? "Revoking"
                  : "Revoke key"
                : busy
                  ? "Deleting"
                  : "Delete key"}
            </button>
          </div>
        </Modal>
      )}
    </div>
  );
}

function Meter({
  label,
  used,
  limit,
}: {
  label: string;
  used: number;
  limit: number;
}) {
  const full = limit > 0 && used >= limit;
  return (
    <div className={full ? "is-full" : ""}>
      <div className="account-allowance">
        <span>{label}</span>
        <strong>
          {limit > 0
            ? `${number(used)} of ${number(limit)}`
            : `${number(used)} used`}
        </strong>
      </div>
      {limit > 0 ? (
        <div
          className="progress"
          role="progressbar"
          aria-label={`${label} used`}
          aria-valuemin={0}
          aria-valuemax={limit}
          aria-valuenow={Math.min(used, limit)}
        >
          <span style={{ width: `${usedShare(used, limit)}%` }} />
        </div>
      ) : (
        <p>No limit</p>
      )}
    </div>
  );
}

// Creates a key or edits one. After a create it shows the secret, which exists
// only in this component's state and is gone once the dialog closes.
function KeyDialog({
  target,
  request,
  copy,
  onSaved,
  onClose,
}: {
  target: APIKey | null;
  request: APIRequest;
  copy: (value: string) => Promise<boolean>;
  onSaved: (key: APIKey, created: boolean) => void;
  onClose: () => void;
}) {
  const [name, setName] = useState(target?.name ?? "");
  const [requests, setRequests] = useState(
    limitText(target?.limitRequests ?? 0),
  );
  const [tokens, setTokens] = useState(limitText(target?.limitTokens ?? 0));
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [secret, setSecret] = useState("");
  const [copied, setCopied] = useState(false);
  const requestLimit = parseLimit(requests);
  const tokenLimit = parseLimit(tokens);
  const requestError = "error" in requestLimit ? requestLimit.error : "";
  const tokenError = "error" in tokenLimit ? tokenLimit.error : "";

  async function save() {
    if (saving || !name.trim()) return;
    if ("error" in requestLimit || "error" in tokenLimit) return;
    setSaving(true);
    setError("");
    const body = {
      name: name.trim(),
      limitRequests: requestLimit.value,
      limitTokens: tokenLimit.value,
    };
    try {
      if (target) {
        const result = await request<{ key: APIKey }>(
          `/api/keys/${encodeURIComponent(target.id)}`,
          "PATCH",
          body,
        );
        onSaved(result.key, false);
        onClose();
      } else {
        const result = await request<{ key: APIKey; secret: string }>(
          "/api/keys",
          "POST",
          body,
        );
        onSaved(result.key, true);
        setSecret(result.secret);
      }
    } catch (err) {
      if (isStale(err)) return;
      setError(
        errorMessage(
          err,
          target ? "Could not save the key." : "Could not create the key.",
        ),
      );
    } finally {
      setSaving(false);
    }
  }

  if (secret)
    return (
      <Modal
        className="key-dialog"
        labelledBy="key-dialog-title"
        locked
        onClose={onClose}
      >
        <h2 id="key-dialog-title">Copy your new key</h2>
        <p className="dialog-lead">
          This is the only time vrouter shows it. The server keeps a hash, so
          nobody can read the key back later. If you lose it, revoke it and
          create another.
        </p>
        <div className="copy-field secret-field">
          <code>{secret}</code>
          <button
            className="secondary"
            onClick={async () => setCopied(await copy(secret))}
          >
            {copied ? <Check size={14} /> : <Copy size={14} />}
            {copied ? "Copied" : "Copy key"}
          </button>
        </div>
        <div className="dialog-actions">
          <button className="primary" onClick={onClose}>
            {copied ? "Done" : "I've saved it"}
          </button>
        </div>
      </Modal>
    );

  return (
    <Modal
      className="key-dialog"
      labelledBy="key-dialog-title"
      locked={saving}
      onClose={onClose}
    >
      <h2 id="key-dialog-title">
        {target ? `Edit ${target.name || "key"}` : "Create an API key"}
      </h2>
      <form
        className="fields"
        onSubmit={(e) => {
          e.preventDefault();
          void save();
        }}
      >
        <div className="field">
          <label htmlFor="key-name">Name</label>
          <input
            id="key-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="Who or what uses this key"
            maxLength={80}
            required
            autoFocus
            autoComplete="off"
          />
        </div>
        <div className="field">
          <label htmlFor="key-requests">Request limit</label>
          <input
            id="key-requests"
            inputMode="numeric"
            value={requests}
            onChange={(e) => setRequests(e.target.value)}
            placeholder="No limit"
            aria-invalid={!!requestError}
            aria-describedby="key-requests-help"
            autoComplete="off"
          />
          <p id="key-requests-help" className={requestError ? "invalid" : ""}>
            {requestError ||
              "Each inference call counts once, before vrouter forwards it. Calls that fail still count. Listing models doesn't."}
          </p>
        </div>
        <div className="field">
          <label htmlFor="key-tokens">Token limit</label>
          <input
            id="key-tokens"
            inputMode="numeric"
            value={tokens}
            onChange={(e) => setTokens(e.target.value)}
            placeholder="No limit"
            aria-invalid={!!tokenError}
            aria-describedby="key-tokens-help"
            autoComplete="off"
          />
          <p id="key-tokens-help" className={tokenError ? "invalid" : ""}>
            {tokenError ||
              "vrouter counts tokens after each response ends and refuses new calls once the total reaches the limit. The call that crosses it still completes, so usage can end above this number. It caps measured usage. It is not a spend budget. A key with a token limit runs one call at a time, and other calls get a 429 until that one finishes."}
          </p>
        </div>
        <p className="fields-note">
          Both limits are lifetime totals for this key. They don't reset.
          {target &&
            " Usage so far stays counted, so a limit below it blocks the key."}
        </p>
        {error && (
          <div className="notice error" role="alert">
            {error}
          </div>
        )}
        <div className="dialog-actions">
          <button
            type="button"
            className="secondary"
            disabled={saving}
            onClick={onClose}
          >
            Cancel
          </button>
          <button
            className="primary"
            disabled={saving || !name.trim() || !!requestError || !!tokenError}
          >
            {target
              ? saving
                ? "Saving"
                : "Save changes"
              : saving
                ? "Creating"
                : "Create key"}
          </button>
        </div>
      </form>
    </Modal>
  );
}
