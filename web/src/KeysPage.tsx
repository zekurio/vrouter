import { useCallback, useEffect, useRef, useState } from "react";
import { Ban, Pencil, RefreshCw, Trash2 } from "lucide-react";
import { errorMessage, isStale, type APIRequest } from "./api";
import { ConfirmKeyDialog, KeyDialog, type Confirm } from "./KeyDialog";
import { keyState, stateLabel, type APIKey } from "./keys";
import { OwnerTabs, ownerQuery, type Owner } from "./OwnerTabs";
import {
  keyBody,
  keyCell,
  keyHead,
  keyRowHead,
  keyStatus,
  keyTable,
  pageNote,
  pageToolbar,
  rowActions,
} from "./pageStyles";
import { Private } from "./Privacy";
import {
  cx,
  empty,
  iconButton,
  loading as loadingClass,
  notice,
  noticeAction,
  panelFrame,
  tableScroll,
} from "./ui";

type Props = {
  // Admins can list, revoke and delete everyone's keys.
  admin: boolean;
  userId: string;
  request: APIRequest;
  reloadKey: string;
  // The page heading owns the create button.
  creating: boolean;
  onCreateClose: () => void;
  onChanged: () => void;
  copy: (value: string) => Promise<boolean>;
  notify: (text: string) => void;
};

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

// Loads the key list again whenever reloadKey or the owner changes.
function useKeys(request: APIRequest, reloadKey: string, owner: Owner) {
  const [keys, setKeys] = useState<APIKey[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const sequence = useRef(0);

  const load = useCallback(async () => {
    const current = ++sequence.current;
    setLoading(true);
    try {
      const query = ownerQuery(owner);
      const result = await request<{ keys: APIKey[] | null }>(
        `/api/keys${query ? `?${query}` : ""}`,
      );
      if (current !== sequence.current) return;
      setKeys(result.keys ?? []);
      setError("");
    } catch (err) {
      if (current !== sequence.current || isStale(err)) return;
      setError(errorMessage(err, "Could not load API keys."));
    } finally {
      if (current === sequence.current) setLoading(false);
    }
  }, [request, owner]);
  useEffect(() => {
    void load();
  }, [reloadKey, load]);

  return { keys, setKeys, loading, error, load };
}

export function KeysPage({
  admin,
  userId,
  request,
  reloadKey,
  creating,
  onCreateClose,
  onChanged,
  copy,
  notify,
}: Props) {
  const [owner, setOwner] = useState<Owner>("mine");
  const { keys, setKeys, loading, error, load } = useKeys(
    request,
    reloadKey,
    owner,
  );
  const [editing, setEditing] = useState<APIKey | null>(null);
  const [confirm, setConfirm] = useState<Confirm | null>(null);
  const [busy, setBusy] = useState(false);
  const [confirmError, setConfirmError] = useState("");

  // A saved key keeps the owner the list showed for it.
  const put = (key: APIKey) =>
    setKeys((list) =>
      list !== null && list.some((k) => k.id === key.id)
        ? list.map((k) => (k.id === key.id ? { ...key, owner: k.owner } : k))
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
    <div>
      {admin && <KeysToolbar owner={owner} setOwner={setOwner} />}
      {keys?.length === 0 && (
        <p className={cx(empty, "mt-6 mb-1")}>No keys yet</p>
      )}
      {error && (
        <div className={cx(notice({ error: true }), "my-3.75")} role="alert">
          {error}{" "}
          <button className={noticeAction} onClick={() => void load()}>
            Try again
          </button>
        </div>
      )}
      {loading && !keys && !error && (
        <div className={loadingClass} role="status" aria-label="Loading">
          <RefreshCw size={22} className="animate-spin" />
        </div>
      )}
      {keys !== null && keys.length > 0 && (
        <KeyTable
          keys={keys}
          userId={userId}
          showOwner={owner === "all"}
          onEdit={setEditing}
          onConfirm={setConfirm}
        />
      )}
      {keys !== null && keys.length > 0 && (
        <p className="mt-3.5 max-w-[68ch] text-[13px] leading-[1.6] text-muted">
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

function KeysToolbar({
  owner,
  setOwner,
}: {
  owner: Owner;
  setOwner: (owner: Owner) => void;
}) {
  return (
    <div className={pageToolbar}>
      <OwnerTabs owner={owner} setOwner={setOwner} label="Show keys of" />
      {owner === "all" && (
        <p className={pageNote}>
          You can revoke or delete anyone&apos;s key. Only its owner can edit
          it.
        </p>
      )}
    </div>
  );
}

const columns = [
  "Key",
  "Owner",
  "Prefix",
  "Status",
  "Requests",
  "Tokens",
  "Created",
  "Expires",
];
const numeric = new Set(["Requests", "Tokens"]);

function KeyTable({
  keys,
  userId,
  showOwner,
  onEdit,
  onConfirm,
}: {
  keys: APIKey[];
  userId: string;
  showOwner: boolean;
  onEdit: (key: APIKey) => void;
  onConfirm: (confirm: Confirm) => void;
}) {
  return (
    <div
      className={cx(
        panelFrame,
        tableScroll,
        "px-5.5 pt-4 pb-1.5 max-sm:px-4.5",
      )}
    >
      <table className={keyTable}>
        <thead>
          <tr>
            {columns
              .filter((c) => showOwner || c !== "Owner")
              .map((c) => (
                <th
                  key={c}
                  scope="col"
                  className={keyHead(numeric.has(c) ? "numeric" : "left")}
                >
                  {c}
                </th>
              ))}
            <th scope="col" className={keyHead()}>
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        {keys.map((key) => (
          <KeyRow
            key={key.id}
            apiKey={key}
            showOwner={showOwner}
            // Admins see everyone's keys but edit only their own.
            editable={!key.owner || key.owner.id === userId}
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
  showOwner,
  editable,
  onEdit,
  onConfirm,
}: {
  apiKey: APIKey;
  showOwner: boolean;
  editable: boolean;
  onEdit: (key: APIKey) => void;
  onConfirm: (confirm: Confirm) => void;
}) {
  const state = keyState(key);
  const dead = state === "revoked";
  const label = key.name || "Unnamed key";
  const off = state !== "active";
  return (
    <tbody className={keyBody}>
      <tr>
        <th scope="row" className={keyRowHead(off)}>
          {label}
        </th>
        {showOwner && (
          <td className={keyCell("left", off)}>
            <Private peek>{key.owner?.name ?? ""}</Private>
          </td>
        )}
        <td className={keyCell("left", off)}>
          <code className="text-muted">{key.prefix}…</code>
        </td>
        <td
          className={keyStatus(state)}
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
        <td className={keyCell("numeric", off)}>{number(key.usedRequests)}</td>
        <td className={keyCell("numeric", off)}>{number(key.usedTokens)}</td>
        <td className={keyCell("left", off)}>{day(key.createdAt)}</td>
        <td className={keyCell("left", off)}>
          {key.expiresAt === undefined || key.expiresAt === ""
            ? "No expiry"
            : moment(key.expiresAt)}
        </td>
        <td className={keyCell()}>
          <div className={rowActions}>
            <button
              className={iconButton()}
              aria-label={`Edit ${label}`}
              title={
                editable
                  ? "Edit name and expiry"
                  : "Only the key's owner can edit it"
              }
              disabled={dead || !editable}
              onClick={() => onEdit(key)}
            >
              <Pencil size={14} />
            </button>
            <button
              className={iconButton("md", { danger: true })}
              aria-label={`Revoke ${label}`}
              title="Revoke"
              disabled={dead}
              onClick={() => onConfirm({ action: "revoke", key })}
            >
              <Ban size={14} />
            </button>
            <button
              className={iconButton("md", { danger: true })}
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
