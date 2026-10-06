import { useState } from "react";
import { Plus } from "lucide-react";
import { errorMessage, type APIRequest, type Gateway } from "./api";
import { Modal } from "./Modal";
import { Select } from "./Select";

export function GatewaySwitcher({
  gateways,
  selected,
  onSelect,
  onCreate,
  canCreate,
}: {
  gateways: Gateway[];
  selected: string | null;
  onSelect: (id: string) => void;
  onCreate: () => void;
  canCreate: boolean;
}) {
  return (
    <div className="gateway-switcher">
      <Select
        label="Gateway"
        value={selected ?? ""}
        options={[
          ...(selected ? [] : [{ value: "", label: "Choose a gateway" }]),
          ...gateways.map((g) => ({ value: g.id, label: g.name })),
        ]}
        onChange={(id) => id && onSelect(id)}
      />
      <button
        className="icon-button"
        aria-label="New gateway"
        title={
          canCreate ? "New gateway" : "Gateways can't be created in demo mode"
        }
        disabled={!canCreate}
        onClick={onCreate}
      >
        <Plus size={16} />
      </button>
    </div>
  );
}

export function CreateGatewayDialog({
  request,
  onCreated,
  onClose,
}: {
  request: APIRequest;
  onCreated: (gateway: Gateway) => void;
  onClose: () => void;
}) {
  const [name, setName] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  async function create() {
    if (saving || !name.trim()) return;
    setSaving(true);
    setError("");
    try {
      const result = await request<{ gateway: Gateway }>(
        "/api/gateways",
        "POST",
        { name: name.trim() },
      );
      onCreated(result.gateway);
    } catch (err) {
      setError(errorMessage(err, "Could not create the gateway."));
      setSaving(false);
    }
  }

  return (
    <Modal
      className="key-dialog"
      labelledBy="gateway-dialog-title"
      locked={saving}
      onClose={onClose}
    >
      <h2 id="gateway-dialog-title">Create a gateway</h2>
      <p className="dialog-lead">
        A gateway has its own provider accounts, model settings, API keys and
        request log. Nothing carries over from your other gateways. To share
        one, hand out its API keys.
      </p>
      <form
        className="fields"
        onSubmit={(e) => {
          e.preventDefault();
          void create();
        }}
      >
        <div className="field">
          <label htmlFor="gateway-name">Name</label>
          <input
            id="gateway-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            maxLength={80}
            required
            autoFocus
            autoComplete="off"
          />
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
          <button className="primary" disabled={saving || !name.trim()}>
            {saving ? "Creating" : "Create gateway"}
          </button>
        </div>
      </form>
    </Modal>
  );
}
