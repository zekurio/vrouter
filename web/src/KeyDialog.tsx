import { useState } from "react";
import { Check, Copy } from "lucide-react";
import { errorMessage, isStale, type APIRequest } from "./api";
import { Modal } from "./Modal";
import { expiryText, parseExpiry, type APIKey } from "./keys";

// Creates a key or edits one. After a create it shows the secret, which exists
// only in this component's state and is gone once the dialog closes.
export function KeyDialog({
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
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [secret, setSecret] = useState("");
  // An untouched field keeps the stored expiry, which may already have passed.
  const expiresAt =
    expiry === storedExpiry
      ? { value: target?.expiresAt ?? null }
      : parseExpiry(expiry);
  const expiryError = "error" in expiresAt ? expiresAt.error : "";

  async function save() {
    if (saving || !name.trim()) return;
    if ("error" in expiresAt) return;
    setSaving(true);
    setError("");
    const body = {
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

  // One dialog for both views, so it stays open when the secret replaces the form.
  return (
    <Modal
      className="key-dialog"
      labelledBy="key-dialog-title"
      locked={secret ? true : saving}
      onClose={onClose}
    >
      {secret ? (
        <NewKeySecret secret={secret} copy={copy} onClose={onClose} />
      ) : (
        <KeyForm
          target={target}
          name={name}
          setName={setName}
          expiry={expiry}
          setExpiry={setExpiry}
          expiryError={expiryError}
          error={error}
          saving={saving}
          onSave={() => void save()}
          onClose={onClose}
        />
      )}
    </Modal>
  );
}

function NewKeySecret({
  secret,
  copy,
  onClose,
}: {
  secret: string;
  copy: (value: string) => Promise<boolean>;
  onClose: () => void;
}) {
  const [copied, setCopied] = useState(false);
  const copySecret = async () => {
    setCopied(await copy(secret));
  };
  return (
    <>
      <h2 id="key-dialog-title">Copy your new key</h2>
      <p className="dialog-lead">
        vrouter shows this key once. If you lose it, revoke it and create
        another.
      </p>
      <div className="copy-field secret-field">
        <code>{secret}</code>
      </div>
      <div className="dialog-actions">
        <button className="secondary" onClick={() => void copySecret()}>
          {copied ? <Check size={14} /> : <Copy size={14} />}
          {copied ? "Copied" : "Copy key"}
        </button>
        <button className="primary" onClick={onClose}>
          {copied ? "Done" : "I've saved it"}
        </button>
      </div>
    </>
  );
}

function KeyForm({
  target,
  name,
  setName,
  expiry,
  setExpiry,
  expiryError,
  error,
  saving,
  onSave,
  onClose,
}: {
  target: APIKey | null;
  name: string;
  setName: (name: string) => void;
  expiry: string;
  setExpiry: (expiry: string) => void;
  expiryError: string;
  error: string;
  saving: boolean;
  onSave: () => void;
  onClose: () => void;
}) {
  return (
    <>
      <h2 id="key-dialog-title">
        {target ? `Edit ${target.name || "key"}` : "Create an API key"}
      </h2>
      <form
        className="fields"
        onSubmit={(e) => {
          e.preventDefault();
          onSave();
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
            // oxlint-disable-next-line jsx-a11y/no-autofocus -- initial focus inside a modal dialog
            autoFocus
            autoComplete="off"
          />
        </div>
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
            disabled={saving || !name.trim() || !!expiryError}
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
    </>
  );
}
