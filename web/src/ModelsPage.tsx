import { useEffect, useState } from "react";
import { RefreshCw, Search, X } from "lucide-react";
import { providerName } from "./ProviderBrand";
import type { APIRequest } from "./api";
import { ModelDialog } from "./ModelDialog";
import { ModelGroup, type RowContext } from "./ModelGroup";
import { filterModels, keyOf, type Model, type Status } from "./models";
import type { Protocol } from "./protocols";
import { Select, type SelectOption } from "./Select";
import { useDirtyReport, useModelSettings } from "./useModelSettings";

const statuses: SelectOption<Status>[] = [
  { value: "all", label: "All models" },
  { value: "enabled", label: "Enabled" },
  { value: "disabled", label: "Disabled" },
  { value: "changed", label: "Unsaved" },
];

export function ModelsPage({
  models,
  request,
  active,
  reloadKey,
  endpoint,
  copy,
  notify,
  onSaved,
  onDirty,
  onAddAccount,
}: {
  models: Model[];
  request: APIRequest;
  // False while another page is shown. The page stays mounted to keep drafts.
  active: boolean;
  reloadKey: string;
  endpoint: string;
  copy: (value: string) => Promise<boolean>;
  notify: (text: string) => void;
  onSaved: () => void;
  // Reports unsaved drafts, and clears the report when the page unmounts.
  onDirty: (dirty: boolean) => void;
  onAddAccount: () => void;
}) {
  const data = useModelSettings({
    models,
    request,
    reloadKey,
    notify,
    onSaved,
  });
  const { rows, view, loading } = data;
  const [search, setSearch] = useState("");
  const [provider, setProvider] = useState("all");
  const [status, setStatus] = useState<Status>("all");
  const [selected, setSelected] = useState<string | null>(null);
  const [copied, setCopied] = useState("");
  const [protocol, setProtocol] = useState<Protocol>("responses");

  useDirtyReport(view.changed.length > 0, onDirty);
  // A modal left open inside the hidden page would block the page shown.
  useEffect(() => {
    if (!active) setSelected(null);
  }, [active]);
  useEffect(() => {
    if (!copied) return undefined;
    const timer = setTimeout(() => setCopied(""), 1800);
    return () => clearTimeout(timer);
  }, [copied]);

  async function copyID(id: string) {
    if (await copy(id)) setCopied(id);
  }

  const providers = [...new Set(rows.map((m) => m.provider))].toSorted((a, b) =>
    providerName(a).localeCompare(providerName(b)),
  );
  const filtered = filterModels(rows, view, { search, provider, status });
  const detail = rows.find((m) => keyOf(m) === selected) ?? null;
  const rowContext: RowContext = {
    view,
    editable: data.editable,
    saving: data.saving,
    copied,
    onCopyID: (id) => void copyID(id),
    onSelect: setSelected,
    onEdit: data.edit,
    onRevert: data.revert,
  };

  return (
    <>
      {data.loadError && (
        <div className="notice error" role="alert">
          {data.loadError}
          <button disabled={loading} onClick={() => void data.load()}>
            Try again
          </button>
        </div>
      )}
      <ModelsToolbar
        search={search}
        setSearch={setSearch}
        providers={providers}
        provider={provider}
        setProvider={setProvider}
        status={status}
        setStatus={setStatus}
      />
      {providers.map((p) => {
        const group = filtered.filter((m) => m.provider === p);
        if (group.length === 0) return null;
        return (
          <ModelGroup
            key={p}
            provider={p}
            group={group}
            all={rows.filter((m) => m.provider === p)}
            rows={rowContext}
            onSetEnabled={data.setEnabled}
          />
        );
      })}
      {filtered.length === 0 &&
        (loading && rows.length === 0 ? (
          <div className="loading" role="status" aria-label="Loading">
            <RefreshCw size={22} className="spinning" />
          </div>
        ) : (
          <NoModels
            filtered={rows.length > 0}
            onClearFilters={() => {
              setSearch("");
              setProvider("all");
              setStatus("all");
            }}
            onAddAccount={onAddAccount}
          />
        ))}
      {view.changed.length > 0 && (
        <SaveBar
          changed={view.changed.length}
          blocked={view.blocked}
          loading={loading}
          saving={data.saving}
          saveError={data.saveError}
          conflict={data.conflict}
          onReload={() => void data.load()}
          onRevert={() => data.revert()}
          onSave={() => void data.save()}
        />
      )}
      <ModelDialog
        selected={selected}
        detail={detail}
        view={view}
        endpoint={endpoint}
        protocol={protocol}
        onProtocol={setProtocol}
        copied={copied}
        onCopyID={(id) => void copyID(id)}
        copy={copy}
        onClose={() => setSelected(null)}
      />
    </>
  );
}

function ModelsToolbar({
  search,
  setSearch,
  providers,
  provider,
  setProvider,
  status,
  setStatus,
}: {
  search: string;
  setSearch: (search: string) => void;
  providers: string[];
  provider: string;
  setProvider: (provider: string) => void;
  status: Status;
  setStatus: (status: Status) => void;
}) {
  return (
    <div className="models-toolbar">
      <label className="search">
        <Search size={15} />
        <input
          aria-label="Search models"
          placeholder="Search models"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        {search && (
          <button
            className="icon-button"
            aria-label="Clear search"
            onClick={() => setSearch("")}
          >
            <X size={14} />
          </button>
        )}
      </label>
      {providers.length > 1 && (
        <div className="tabs" aria-label="Filter by provider">
          {["all", ...providers].map((p) => (
            <button
              key={p}
              aria-pressed={provider === p}
              className={provider === p ? "selected" : ""}
              onClick={() => setProvider(p)}
            >
              {p === "all" ? "All" : providerName(p)}
            </button>
          ))}
        </div>
      )}
      <Select
        label="Filter by status"
        value={status}
        options={statuses}
        onChange={setStatus}
      />
    </div>
  );
}

function NoModels({
  filtered,
  onClearFilters,
  onAddAccount,
}: {
  // True when there are models, but none match the filters.
  filtered: boolean;
  onClearFilters: () => void;
  onAddAccount: () => void;
}) {
  return (
    <div className="empty">
      <p>{filtered ? "No matching models" : "No models yet"}</p>
      {filtered ? (
        <button className="secondary" onClick={onClearFilters}>
          Clear filters
        </button>
      ) : (
        <button className="secondary" onClick={onAddAccount}>
          Add account
        </button>
      )}
    </div>
  );
}

function SaveBar({
  changed,
  blocked,
  loading,
  saving,
  saveError,
  conflict,
  onReload,
  onRevert,
  onSave,
}: {
  changed: number;
  // Duplicate exposed IDs keep the changes from being saved.
  blocked: boolean;
  loading: boolean;
  saving: boolean;
  saveError: string;
  conflict: boolean;
  onReload: () => void;
  onRevert: () => void;
  onSave: () => void;
}) {
  return (
    <div className="save-bar" role="region" aria-label="Unsaved changes">
      <p>
        <strong>
          {changed} unsaved {changed === 1 ? "change" : "changes"}
        </strong>
        {saveError ? (
          <span className="save-error" role="alert">
            {saveError}
          </span>
        ) : (
          blocked && (
            <span className="save-error">Resolve duplicate IDs first.</span>
          )
        )}
      </p>
      {conflict && (
        <button className="secondary" disabled={loading} onClick={onReload}>
          Reload settings
        </button>
      )}
      <button className="secondary" disabled={saving} onClick={onRevert}>
        Revert
      </button>
      <button
        className="primary"
        disabled={saving || blocked || conflict}
        onClick={onSave}
      >
        {saving ? "Saving" : "Save changes"}
      </button>
    </div>
  );
}
