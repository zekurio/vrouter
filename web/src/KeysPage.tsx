import { useCallback, useEffect, useRef, useState } from "react";
import { Ban, Pencil, RefreshCw, Trash2 } from "lucide-react";
import { errorMessage, isStale, type APIRequest } from "./api";
import { Modal } from "./Modal";
import { KeyDialog } from "./KeyDialog";
import { keyState, stateLabel, type APIKey } from "./keys";

type Props = {
  request: APIRequest;
  reloadKey: string;
  // The page heading owns the create button.
  creating: boolean;
  onCreateClose: () => void;
  onChanged: () => void;
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

// Loads the key list again whenever reloadKey changes.
function useKeys(request: APIRequest, reloadKey: string) {
  const [keys, setKeys] = useState<APIKey[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const sequence = useRef(0);

  const load = useCallback(async () => {
    const current = ++sequence.current;
    setLoading(true);
    try {
      const result = await request<{ keys: APIKey[] | null }>("/api/keys");
      if (current !== sequence.current) return;
      setKeys(result.keys ?? []);
      setError("");
    } catch (err) {
      if (current !== sequence.current || isStale(err)) return;
      setError(errorMessage(err, "Could not load API keys."));
    } finally {
      if (current === sequence.current) setLoading(false);
    }
  }, [request]);
  useEffect(() => {
    void load();
  }, [reloadKey, load]);

  return { keys, setKeys, loading, error, load };
}

export function KeysPage({
  request,
  reloadKey,
  creating,
  onCreateClose,
  onChanged,
  copy,
  notify,
}: Props) {
  const { keys, setKeys, loading, error, load } = useKeys(request, reloadKey);
  const [editing, setEditing] = useState<APIKey | null>(null);
  const [confirm, setConfirm] = useState<Confirm | null>(null);
  const [busy, setBusy] = useState(false);
  const [confirmError, setConfirmError] = useState("");

  const put = (key: APIKey) =>
    setKeys((list) =>
      list !== null && list.some((k) => k.id === key.id)
        ? list.map((k) => (k.id === key.id ? key : k))
        : [key, ...(list ?? [])],
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
      onChanged();
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
      {keys?.length === 0 && <p className="empty">No keys yet</p>}
      {error && (
        <div className="notice error" role="alert">
          {error} <button onClick={() => void load()}>Try again</button>
        </div>
      )}
      {loading && !keys && !error && (
        <div className="loading" role="status" aria-label="Loading">
          <RefreshCw size={22} className="spinning" />
        </div>
      )}
      {keys !== null && keys.length > 0 && (
        <KeyTable keys={keys} onEdit={setEditing} onConfirm={setConfirm} />
      )}
      {keys !== null && keys.length > 0 && (
        <p className="keys-footnote">
          Request and token counts are totals since the key was created.
        </p>
      )}
      {(editing !== null || creating) && (
        <KeyDialog
          target={editing}
          request={request}
          copy={copy}
          onSaved={(key, created) => {
            put(key);
            onChanged();
            if (!created) notify("Key updated");
          }}
          onClose={() => {
            setEditing(null);
            onCreateClose();
          }}
        />
      )}
      {confirm && (
        <ConfirmKeyDialog
          confirm={confirm}
          busy={busy}
          error={confirmError}
          onClose={closeConfirm}
          onConfirm={() => void runConfirm()}
        />
      )}
    </div>
  );
}

function KeyTable({
  keys,
  onEdit,
  onConfirm,
}: {
  keys: APIKey[];
  onEdit: (key: APIKey) => void;
  onConfirm: (confirm: Confirm) => void;
}) {
  return (
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
        {keys.map((key) => (
          <KeyRow
            key={key.id}
            apiKey={key}
            onEdit={onEdit}
            onConfirm={onConfirm}
          />
        ))}
      </table>
    </div>
  );
}

function KeyRow({
  apiKey: key,
  onEdit,
  onConfirm,
}: {
  apiKey: APIKey;
  onEdit: (key: APIKey) => void;
  onConfirm: (confirm: Confirm) => void;
}) {
  const state = keyState(key);
  const dead = state === "revoked";
  const label = key.name || "Unnamed key";
  return (
    <tbody className={state === "active" ? "" : "is-off"}>
      <tr>
        <th scope="row">{label}</th>
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
          {key.expiresAt === undefined || key.expiresAt === ""
            ? "No expiry"
            : moment(key.expiresAt)}
        </td>
        <td>
          <div className="account-row-actions">
            <button
              className="icon-button"
              aria-label={`Edit ${label}`}
              title="Edit name and expiry"
              disabled={dead}
              onClick={() => onEdit(key)}
            >
              <Pencil size={14} />
            </button>
            <button
              className="icon-button is-danger"
              aria-label={`Revoke ${label}`}
              title="Revoke"
              disabled={dead}
              onClick={() => onConfirm({ action: "revoke", key })}
            >
              <Ban size={14} />
            </button>
            <button
              className="icon-button is-danger"
              aria-label={`Delete ${label}`}
              title="Delete"
              onClick={() => onConfirm({ action: "delete", key })}
            >
              <Trash2 size={15} />
            </button>
          </div>
        </td>
      </tr>
    </tbody>
  );
}

function ConfirmKeyDialog({
  confirm,
  busy,
  error,
  onClose,
  onConfirm,
}: {
  confirm: Confirm;
  busy: boolean;
  error: string;
  onClose: () => void;
  onConfirm: () => void;
}) {
  return (
    <Modal
      className="confirm-dialog"
      labelledBy="key-confirm-title"
      locked={busy}
      onClose={onClose}
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
      {error && (
        <div className="notice error" role="alert">
          {error}
        </div>
      )}
      <div className="dialog-actions">
        <button className="secondary" disabled={busy} onClick={onClose}>
          Keep key
        </button>
        <button className="primary danger" disabled={busy} onClick={onConfirm}>
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
  );
}
