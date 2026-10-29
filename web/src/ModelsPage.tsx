import { useEffect, useState } from "react";
import { RefreshCw, Search, X } from "lucide-react";
import { providerName } from "./ProviderBrand";
import type { APIRequest } from "./api";
import { ModelDialog, type Protocol } from "./ModelDialog";
import { ModelGroup, type RowContext } from "./ModelGroup";
import { filterModels, keyOf, type Model, type Status } from "./models";
import { Select, type SelectOption } from "./Select";
import {
  button,
  cx,
  empty,
  iconButton,
  loading as loadingClass,
  notice,
  noticeAction,
  tab,
  tabs,
} from "./ui";
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
  editable,
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
  // Model settings apply to everyone, so only admins change them.
  editable: boolean;
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
    editable: data.editable && editable,
    saving: data.saving,
    copied,
    onCopyID: (id) => void copyID(id),
    onSelect: setSelected,
    onEdit: data.edit,
    onRevert: data.revert,
  };

  return (
    <>
      <ModelsNotices
        loadError={data.loadError}
        loading={loading}
        editable={editable}
        onRetry={() => void data.load()}
      />
      <ModelsToolbar
        search={search}
        setSearch={setSearch}
        providers={providers}
        provider={provider}
        setProvider={setProvider}
        status={status}
        setStatus={setStatus}
      />
      {providers
        .filter((p) => filtered.some((m) => m.provider === p))
        .map((p, i) => {
          const group = filtered.filter((m) => m.provider === p);
          return (
            <ModelGroup
              key={p}
              first={i === 0}
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
          <div className={loadingClass} role="status" aria-label="Loading">
            <RefreshCw size={22} className="animate-spin" />
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

function ModelsNotices({
  loadError,
  loading,
  editable,
  onRetry,
}: {
  loadError: string;
  loading: boolean;
  editable: boolean;
  onRetry: () => void;
}) {
  return (
    <>
      {loadError && (
        <div className={cx(notice({ error: true }), "my-3.75")} role="alert">
          {loadError}
          <button className={noticeAction} disabled={loading} onClick={onRetry}>
            Try again
          </button>
        </div>
      )}
      {!editable && (
        <div className={cx(notice(), "my-3.75")}>
          Model settings apply to everyone. Only admins can change them.
        </div>
      )}
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
    // Below 720px the search takes its own row.
    <div className="mb-3 flex items-center gap-2 max-md:flex-wrap">
      {/* In the light theme the border stays even while typing. */}
      <label className="flex h-9 min-w-0 flex-1 items-center gap-2.25 rounded-[10px] border border-transparent bg-surface pr-1 pl-2.75 text-muted focus-within:border-accent light:border-line max-md:basis-full">
        <Search size={15} />
        <input
          className="h-full w-full min-w-0 px-0.5 py-px text-[13px] text-text outline-none placeholder:text-muted"
          aria-label="Search models"
          placeholder="Search models"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        {search && (
          <button
            className={iconButton("sm")}
            aria-label="Clear search"
            onClick={() => setSearch("")}
          >
            <X size={14} />
          </button>
        )}
      </label>
      {providers.length > 1 && (
        <div
          className={cx(tabs(), "max-md:flex-1 max-md:overflow-x-auto")}
          aria-label="Filter by provider"
        >
          {["all", ...providers].map((p) => (
            <button
              key={p}
              aria-pressed={provider === p}
              className={tab(provider === p)}
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
    <div className={cx(empty, "mt-6 mb-1")}>
      <p>{filtered ? "No matching models" : "No models yet"}</p>
      {filtered ? (
        <button className={button("secondary")} onClick={onClearFilters}>
          Clear filters
        </button>
      ) : (
        <button className={button("secondary")} onClick={onAddAccount}>
          Add account
        </button>
      )}
    </div>
  );
}

const saveErrorClass = "text-danger wrap-anywhere";

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
    // Phones lift it over the navigation bar pinned to the bottom edge.
    <div
      className="sticky bottom-4.5 z-10 mt-7 flex items-center gap-2 rounded-[14px] border border-line bg-surface py-2.5 pr-2.5 pl-4.5 shadow-[0_12px_40px_#0008] max-sm:bottom-19 max-sm:flex-wrap max-sm:p-3"
      role="region"
      aria-label="Unsaved changes"
    >
      <p className="flex min-w-0 flex-1 flex-wrap gap-x-3 gap-y-0.5 text-[13px] max-sm:mb-1 max-sm:basis-full">
        <strong className="font-medium whitespace-nowrap">
          {changed} unsaved {changed === 1 ? "change" : "changes"}
        </strong>
        {saveError ? (
          <span className={saveErrorClass} role="alert">
            {saveError}
          </span>
        ) : (
          blocked && (
            <span className={saveErrorClass}>Resolve duplicate IDs first.</span>
          )
        )}
      </p>
      {conflict && (
        <button
          className={cx(button("secondary"), "max-sm:flex-1")}
          disabled={loading}
          onClick={onReload}
        >
          Reload settings
        </button>
      )}
      <button
        className={cx(button("secondary"), "max-sm:flex-1")}
        disabled={saving}
        onClick={onRevert}
      >
        Revert
      </button>
      <button
        className={cx(button("primary"), "max-sm:flex-1")}
        disabled={saving || blocked || conflict}
        onClick={onSave}
      >
        {saving ? "Saving" : "Save changes"}
      </button>
    </div>
  );
}
