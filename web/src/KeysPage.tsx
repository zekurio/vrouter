import { useCallback, useEffect, useRef, useState } from "react";
import { Ban, Check, Copy, Pencil, RefreshCw, Trash2 } from "lucide-react";
import { errorMessage, isStale, type APIRequest } from "./api";
import { providerLabel } from "./ProviderBrand";
import { Modal } from "./Modal";
import {
  expiryText,
  keyState,
  parseExpiry,
  stateLabel,
  type APIKey,
} from "./keys";

type Props = {
  request: APIRequest;
  live: boolean;
  reloadKey: string;
  // The page heading owns the create button.
  creating: boolean;
  onCreateClose: () => void;
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
const moment = (value: string) =>
  new Date(value).toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });

export function KeysPage({
  request,
  live,
  reloadKey,
  creating,
  onCreateClose,
  copy,
  notify,
}: Props) {
  const [keys, setKeys] = useState<APIKey[] | null>(null);
  const [loading, setLoading] = useState(live);
  const [error, setError] = useState("");
  const [editing, setEditing] = useState<APIKey | null>(null);
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
      {live && keys?.length === 0 && <p className="empty">No keys yet</p>}
      {!live && (
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
        <div className="loading">
          <RefreshCw size={22} className="spinning" />
        </div>
      )}
      {live && !!keys?.length && (
        <div className="table-scroll">
          <table className="key-table">
            <thead>
              <tr>
                <th scope="col">Key</th>
                <th scope="col">Prefix</th>
                <th scope="col">Status</th>
                <th scope="col" className="numeric">
                  Requests
                </th>
                <th scope="col" className="numeric">
                  Tokens
                </th>
                <th scope="col">Created</th>
                <th scope="col">Expires</th>
                <th scope="col">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            {keys.map((key) => {
              const state = keyState(key);
              const dead = state === "revoked";
              const quotas = Object.entries(key.providerQuotas ?? {}).filter(
                ([, q]) => q.fiveHour != null || q.sevenDay != null,
              );
              return (
                <tbody
                  className={state !== "active" ? "is-off" : ""}
                  key={key.id}
                >
                  <tr>
                    <th scope="row">{key.name || "Unnamed key"}</th>
                    <td>
                      <code>{key.prefix}…</code>
                    </td>
                    <td
                      className={`key-status ${state}`}
                      title={
                        state === "revoked"
                          ? `Revoked ${day(key.revokedAt!)}. Delete it to remove it from this list.`
                          : state === "expired"
                            ? "Edit it to set a later expiry or clear it."
                            : undefined
                      }
                    >
                      {stateLabel[state]}
                    </td>
                    <td className="numeric">{number(key.usedRequests)}</td>
                    <td className="numeric">{number(key.usedTokens)}</td>
                    <td>{day(key.createdAt)}</td>
                    <td>
                      {key.expiresAt ? moment(key.expiresAt) : "No expiry"}
                    </td>
                    <td>
                      <div className="account-row-actions">
                        <button
                          className="icon-button"
                          aria-label={`Edit ${key.name}`}
                          title="Edit name, quotas and expiry"
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
                    </td>
                  </tr>
                  {quotas.length > 0 && (
                    <tr className="key-provider-usage">
                      <td colSpan={8}>
                        {quotas.map(([provider, q]) => {
                          const usage = key.providerUsage?.[provider];
                          return (
                            <div key={provider}>
                              <strong>{providerLabel(provider)}</strong>
                              {(["fiveHour", "sevenDay"] as const)
                                .filter((window) => q[window] != null)
                                .map((window) => {
                                  const used = usage?.[window] ?? 0,
                                    limit = q[window]!;
                                  return (
                                    <span
                                      key={window}
                                      className={used >= limit ? "invalid" : ""}
                                    >
                                      {window === "fiveHour" ? "5h" : "7d"}:{" "}
                                      {used.toFixed(2)}% / {limit}%
                                      {used >= limit ? " · Limit reached" : ""}
                                    </span>
                                  );
                                })}
                              {usage?.uncertain && (
                                <span className="invalid">
                                  Blocked: usage incomplete
                                </span>
                              )}
                            </div>
                          );
                        })}
                      </td>
                    </tr>
                  )}
                </tbody>
              );
            })}
          </table>
        </div>
      )}
      {live && !!keys?.length && (
        <p className="keys-footnote">
          Request and token counts are totals since the key was created.
          Provider percentages reset with each account's window.
        </p>
      )}
      {(editing || creating) && (
        <KeyDialog
          target={editing}
          request={request}
          copy={copy}
          onSaved={(key, created) => {
            put(key);
            if (!created) notify("Key updated");
          }}
          onClose={() => {
            setEditing(null);
            onCreateClose();
          }}
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
              ? "vrouter refuses new calls made with this key. Running responses finish. You can't undo this."
              : "vrouter refuses new calls made with this key. Running responses finish, and its past requests stay in the log. You can't undo this."}
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
  const storedExpiry = expiryText(target?.expiresAt);
  const [expiry, setExpiry] = useState(storedExpiry);
  const [percent, setPercent] = useState<
    Record<string, Record<string, string>>
  >(() =>
    Object.fromEntries(
      ["claude", "codex"].map((provider) => [
        provider,
        {
          fiveHour:
            target?.providerQuotas?.[provider]?.fiveHour?.toString() ?? "",
          sevenDay:
            target?.providerQuotas?.[provider]?.sevenDay?.toString() ?? "",
        },
      ]),
    ),
  );
  const percentInvalid = Object.values(percent).some((q) =>
    Object.values(q).some(
      (value) =>
        value.trim() !== "" &&
        (!Number.isFinite(Number(value)) ||
          Number(value) < 0 ||
          Number(value) > 100),
    ),
  );
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [secret, setSecret] = useState("");
  const [copied, setCopied] = useState(false);
  // An untouched field keeps the stored expiry, which may already have passed.
  const expiresAt =
    expiry === storedExpiry
      ? { value: target?.expiresAt ?? null }
      : parseExpiry(expiry);
  const expiryError = "error" in expiresAt ? expiresAt.error : "";

  async function save() {
    if (saving || !name.trim() || percentInvalid) return;
    if ("error" in expiresAt) return;
    setSaving(true);
    setError("");
    const body = {
      providerQuotas: Object.fromEntries(
        Object.entries(percent).map(([provider, fields]) => [
          provider,
          Object.fromEntries(
            Object.entries(fields)
              .filter(([, value]) => value.trim() !== "")
              .map(([window, value]) => [window, Number(value)]),
          ),
        ]),
      ),
      name: name.trim(),
      expiresAt: expiresAt.value,
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
          vrouter shows this key once. If you lose it, revoke it and create
          another.
        </p>
        <div className="copy-field secret-field">
          <code>{secret}</code>
        </div>
        <div className="dialog-actions">
          <button
            className="secondary"
            onClick={async () => setCopied(await copy(secret))}
          >
            {copied ? <Check size={14} /> : <Copy size={14} />}
            {copied ? "Copied" : "Copy key"}
          </button>
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
            maxLength={64}
            required
            autoFocus
            autoComplete="off"
          />
        </div>
        <fieldset className="provider-quota-fields">
          <legend>
            Provider quotas <small>Optional</small>
          </legend>
          <p>
            Percent of the subscription pool. Blank is unlimited, 0 blocks the
            provider. Usage is measured after each response, so the last request
            can pass a limit.
          </p>
          {["claude", "codex"].map((provider) => (
            <div className="provider-quota-inputs" key={provider}>
              <strong>{providerLabel(provider)}</strong>
              {(["fiveHour", "sevenDay"] as const).map((window) => (
                <div className="field" key={window}>
                  <label htmlFor={provider + window}>
                    {window === "fiveHour" ? "5-hour" : "7-day"} allowance (%)
                  </label>
                  <input
                    id={provider + window}
                    type="number"
                    min="0"
                    max="100"
                    step="any"
                    placeholder="No limit"
                    value={percent[provider][window]}
                    onChange={(e) =>
                      setPercent((old) => ({
                        ...old,
                        [provider]: {
                          ...old[provider],
                          [window]: e.target.value,
                        },
                      }))
                    }
                  />
                </div>
              ))}
            </div>
          ))}
          {target &&
            Object.values(target.providerUsage ?? {}).some(
              (u) => u.uncertain,
            ) && (
              <p className="notice">
                Saving these quotas acknowledges incomplete percentage
                accounting and resumes access within the remaining allowance.
              </p>
            )}
        </fieldset>
        <div className="field">
          <label htmlFor="key-expiry">Expires</label>
          <input
            id="key-expiry"
            type="datetime-local"
            value={expiry}
            min={expiryText(new Date().toISOString())}
            onChange={(e) => setExpiry(e.target.value)}
            aria-invalid={!!expiryError}
            aria-describedby="key-expiry-help"
          />
          <p id="key-expiry-help" className={expiryError ? "invalid" : ""}>
            {expiryError ||
              "Blank means it never expires. After this time vrouter refuses new calls."}
          </p>
        </div>
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
            disabled={saving || !name.trim() || percentInvalid || !!expiryError}
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
