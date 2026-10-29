import { useCallback, useEffect, useRef, useState } from "react";
import { RefreshCw } from "lucide-react";
import {
  errorMessage,
  isStale,
  type APIRequest,
  type Role,
  type SessionUser,
} from "./api";
import { Badge, badgeRow, RoleBadge } from "./Badge";
import { Modal } from "./Modal";
import {
  keyBody,
  keyCell,
  keyHead,
  keyRowHead,
  keyStatus,
  keyTable,
  pageNote,
} from "./pageStyles";
import { Private } from "./Privacy";
import {
  button,
  confirmTitle,
  cx,
  dialogActions,
  dialogLead,
  loading as loadingClass,
  notice,
  noticeAction,
  panel,
  tableScroll,
} from "./ui";

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
    <div>
      <p className={cx(pageNote, "mb-4.5")}>
        People appear here after their first sign-in. Roles come from the
        identity provider&apos;s vrouter_role claim and update at every sign-in.
      </p>
      {error && (
        <div className={cx(notice({ error: true }), "my-3.75")} role="alert">
          {error}{" "}
          <button className={noticeAction} onClick={() => void load()}>
            Try again
          </button>
        </div>
      )}
      {loading && !users && !error && (
        <div className={loadingClass} role="status" aria-label="Loading">
          <RefreshCw size={22} className="animate-spin" />
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
    <div className={cx(panel, tableScroll)}>
      <table className={keyTable}>
        <thead>
          <tr>
            <th scope="col" className={keyHead()}>
              User
            </th>
            <th scope="col" className={keyHead()}>
              Role
            </th>
            <th scope="col" className={keyHead()}>
              Status
            </th>
            <th scope="col" className={keyHead("numeric")}>
              Active keys
            </th>
            <th scope="col" className={keyHead()}>
              First sign-in
            </th>
            <th scope="col" className={keyHead()}>
              Last sign-in
            </th>
            <th scope="col" className={keyHead()}>
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody className={keyBody}>
          {users.map((u) => {
            const off = disabled(u);
            return (
              <tr key={u.id}>
                <th scope="row" className={keyRowHead(off)}>
                  <span className={badgeRow}>
                    <Private peek>{userLabel(u)}</Private>
                    {u.id === me && <Badge>You</Badge>}
                  </span>
                  {u.name !== "" && u.email !== "" && (
                    <span className="mt-0.5 block text-[11.5px] font-normal text-muted">
                      <Private peek>{u.email}</Private>
                    </span>
                  )}
                </th>
                <td className={keyCell("left", off)}>
                  <RoleBadge role={u.role} />
                </td>
                <td className={keyStatus(off ? "revoked" : "active")}>
                  {off ? "Disabled" : "Active"}
                </td>
                <td className={keyCell("numeric", off)}>
                  {u.keys.toLocaleString()}
                </td>
                <td className={keyCell("left", off)}>{day(u.createdAt)}</td>
                <td className={keyCell("left", off)}>{day(u.signedInAt)}</td>
                <td className={keyCell("right")}>
                  {u.id !== me && (
                    <button
                      className={button(off ? "secondary" : "danger-text")}
                      disabled={busy}
                      onClick={() => (off ? onEnable(u) : onDisable(u))}
                    >
                      {off ? "Enable" : "Disable"}
                    </button>
                  )}
                </td>
              </tr>
            );
          })}
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
      width="xs"
      labelledBy="user-confirm-title"
      locked={busy}
      onClose={onClose}
    >
      <h2 id="user-confirm-title" className={confirmTitle}>
        Disable <Private>{userLabel(user)}</Private>?
      </h2>
      <p className={dialogLead}>
        vrouter signs them out, refuses their API keys, and stops using their
        personal accounts. Their keys, accounts and usage stay, and enabling
        them again restores access.
      </p>
      {error && (
        <div
          className={cx(notice({ error: true, inset: true }), "mt-4.5")}
          role="alert"
        >
          {error}
        </div>
      )}
      <div className={cx(dialogActions, "mt-6")}>
        <button
          className={button("secondary")}
          disabled={busy}
          onClick={onClose}
        >
          Keep access
        </button>
        <button
          className={button("danger")}
          disabled={busy}
          onClick={onDisable}
        >
          {busy ? "Disabling" : "Disable user"}
        </button>
      </div>
    </Modal>
  );
}
