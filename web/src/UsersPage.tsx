import { useCallback, useEffect, useRef, useState } from "react";
import { RefreshCw } from "lucide-react";
import {
  errorMessage,
  isStale,
  type APIRequest,
  type Role,
  type SessionUser,
} from "./api";
import { Modal } from "./Modal";
import { Private } from "./Privacy";

// A person who has signed in, as the Users page lists them.
type User = {
  id: string;
  // Empty when the identity provider sent none.
  email: string;
  name: string;
  role: Role;
  createdAt: string;
  signedInAt: string;
  disabledAt?: string;
  // Active API keys.
  keys: number;
};

const day = (value: string) =>
  new Date(value).toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
const userLabel = (u: User) => u.name || u.email || "Unnamed user";
const disabled = (u: User) => u.disabledAt !== undefined;

function useUsers(request: APIRequest) {
  const [users, setUsers] = useState<User[] | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const sequence = useRef(0);
  const load = useCallback(async () => {
    const current = ++sequence.current;
    setLoading(true);
    try {
      const result = await request<{ users: User[] }>("/api/users");
      if (current !== sequence.current) return;
      setUsers(result.users);
      setError("");
    } catch (err) {
      if (current !== sequence.current || isStale(err)) return;
      setError(errorMessage(err, "Could not load users."));
    } finally {
      if (current === sequence.current) setLoading(false);
    }
  }, [request]);
  useEffect(() => {
    void load();
  }, [load]);
  return { users, setUsers, error, loading, load };
}

// Everyone who has signed in. Roles follow the identity provider, so the
// page only disables and enables people.
export function UsersPage({
  user: me,
  request,
  notify,
}: {
  user: SessionUser;
  request: APIRequest;
  notify: (text: string) => void;
}) {
  const { users, setUsers, error, loading, load } = useUsers(request);
  const [confirm, setConfirm] = useState<User | null>(null);
  const [busy, setBusy] = useState(false);
  const [confirmError, setConfirmError] = useState("");

  async function setAccess(user: User, disable: boolean) {
    setBusy(true);
    setConfirmError("");
    try {
      const result = await request<{ user: User }>(
        `/api/users/${encodeURIComponent(user.id)}`,
        "PATCH",
        { disabled: disable },
      );
      setUsers(
        (list) =>
          list?.map((u) =>
            u.id === user.id ? { ...result.user, keys: u.keys } : u,
          ) ?? null,
      );
      setConfirm(null);
      notify(disable ? "User disabled" : "User enabled");
    } catch (err) {
      setConfirmError(errorMessage(err, "Could not update the user."));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="users">
      <p className="page-lead">
        People appear here after their first sign-in. Roles come from the
        identity provider&apos;s vrouter_role claim and update at every sign-in.
      </p>
      {error && (
        <div className="notice error" role="alert">
          {error} <button onClick={() => void load()}>Try again</button>
        </div>
      )}
      {loading && !users && !error && (
        <div className="loading" role="status" aria-label="Loading">
          <RefreshCw size={22} className="spinning" />
        </div>
      )}
      {users && users.length > 0 && (
        <UserTable
          users={users}
          me={me.id}
          busy={busy}
          onEnable={(u) => void setAccess(u, false)}
          onDisable={(u) => {
            setConfirmError("");
            setConfirm(u);
          }}
        />
      )}
      {confirm && (
        <DisableDialog
          user={confirm}
          busy={busy}
          error={confirmError}
          onClose={() => {
            if (!busy) setConfirm(null);
          }}
          onDisable={() => void setAccess(confirm, true)}
        />
      )}
    </div>
  );
}

function UserTable({
  users,
  me,
  busy,
  onEnable,
  onDisable,
}: {
  users: User[];
  me: string;
  busy: boolean;
  onEnable: (user: User) => void;
  onDisable: (user: User) => void;
}) {
  return (
    <div className="panel table-scroll">
      <table className="key-table user-table">
        <thead>
          <tr>
            <th scope="col">User</th>
            <th scope="col">Role</th>
            <th scope="col">Status</th>
            <th scope="col" className="numeric">
              Active keys
            </th>
            <th scope="col">First sign-in</th>
            <th scope="col">Last sign-in</th>
            <th scope="col">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {users.map((u) => (
            <tr key={u.id} className={disabled(u) ? "is-off" : ""}>
              <th scope="row">
                <span className="cell-main">
                  <Private peek>{userLabel(u)}</Private>
                  {u.id === me && <span className="you-badge">You</span>}
                </span>
                {u.name !== "" && u.email !== "" && (
                  <span className="cell-sub">
                    <Private peek>{u.email}</Private>
                  </span>
                )}
              </th>
              <td>
                <span className={`role-badge ${u.role}`}>
                  {u.role === "admin" ? "Admin" : "Member"}
                </span>
              </td>
              <td
                className={`key-status ${disabled(u) ? "revoked" : "active"}`}
              >
                {disabled(u) ? "Disabled" : "Active"}
              </td>
              <td className="numeric">{u.keys.toLocaleString()}</td>
              <td>{day(u.createdAt)}</td>
              <td>{day(u.signedInAt)}</td>
              <td>
                {u.id !== me && (
                  <button
                    className={
                      disabled(u) ? "secondary" : "secondary danger-text"
                    }
                    disabled={busy}
                    onClick={() => (disabled(u) ? onEnable(u) : onDisable(u))}
                  >
                    {disabled(u) ? "Enable" : "Disable"}
                  </button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function DisableDialog({
  user,
  busy,
  error,
  onClose,
  onDisable,
}: {
  user: User;
  busy: boolean;
  error: string;
  onClose: () => void;
  onDisable: () => void;
}) {
  return (
    <Modal
      className="confirm-dialog"
      labelledBy="user-confirm-title"
      locked={busy}
      onClose={onClose}
    >
      <h2 id="user-confirm-title">
        Disable <Private>{userLabel(user)}</Private>?
      </h2>
      <p>
        vrouter signs them out, refuses their API keys, and stops using their
        personal accounts. Their keys, accounts and usage stay, and enabling
        them again restores access.
      </p>
      {error && (
        <div className="notice error" role="alert">
          {error}
        </div>
      )}
      <div className="dialog-actions">
        <button className="secondary" disabled={busy} onClick={onClose}>
          Keep access
        </button>
        <button className="primary danger" disabled={busy} onClick={onDisable}>
          {busy ? "Disabling" : "Disable user"}
        </button>
      </div>
    </Modal>
  );
}
