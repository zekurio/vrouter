import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type CSSProperties,
} from "react";
import { Check, Copy, RefreshCw, Search, Undo2, X } from "lucide-react";
import { ProviderBrand, providerColor, providerName } from "./ProviderBrand";
import { errorMessage, isStale, statusOf, type APIRequest } from "./api";
import { CodeBlock } from "./CodeBlock";
import { ConnectionTest } from "./ConnectionTest";
import { isDialogBackdropClick } from "./dialog";
import {
  curlExample,
  protocolPath,
  protocols,
  type Protocol,
} from "./protocols";
import { Select, type SelectOption } from "./Select";

export type Model = {
  id: string;
  name: string;
  provider: string;
  context: number;
  maxOutput?: number;
  created?: number;
  inputs?: string[];
  reasoning?: string[];
  reasoningSupported?: boolean;
};
type ManagedModel = Model & {
  enabled: boolean;
  alias: string;
  readOnly?: string;
};
type ModelSettings = { models: ManagedModel[]; revision: string };
type Draft = { enabled: boolean; alias: string };
type Status = "all" | "enabled" | "disabled" | "changed";
const statuses: SelectOption<Status>[] = [
  { value: "all", label: "All models" },
  { value: "enabled", label: "Enabled" },
  { value: "disabled", label: "Disabled" },
  { value: "changed", label: "Unsaved" },
];
const compact = (n: number) =>
  Intl.NumberFormat("en", {
    notation: "compact",
    maximumFractionDigits: 1,
  }).format(n);
const keyOf = (m: { provider: string; id: string }) => `${m.provider}\n${m.id}`;

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
  const [settings, setSettings] = useState<ModelSettings | null>(null);
  const [loading, setLoading] = useState(false);
  const [loadError, setLoadError] = useState("");
  const [drafts, setDrafts] = useState<Record<string, Draft>>({});
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [conflict, setConflict] = useState(false);
  const [search, setSearch] = useState("");
  const [provider, setProvider] = useState("all");
  const [status, setStatus] = useState<Status>("all");
  const [selected, setSelected] = useState<string | null>(null);
  const [copied, setCopied] = useState("");
  const [protocol, setProtocol] = useState<Protocol>("responses");
  const dialog = useRef<HTMLDialogElement>(null);
  const sequence = useRef(0);

  const load = useCallback(async () => {
    const current = ++sequence.current;
    setLoading(true);
    try {
      const next = await request<ModelSettings>("/api/model-settings");
      if (current !== sequence.current) return;
      if (!next || !Array.isArray(next.models))
        throw new Error("The server returned unreadable model settings.");
      setSettings(next);
      setLoadError("");
      setConflict(false);
      setSaveError("");
    } catch (err) {
      if (current !== sequence.current || isStale(err)) return;
      setLoadError(errorMessage(err, "Could not load model settings."));
    } finally {
      if (current === sequence.current) setLoading(false);
    }
  }, [request]);
  useEffect(() => {
    void load();
  }, [reloadKey, load]);

  const rows: ManagedModel[] =
    settings?.models ?? models.map((m) => ({ ...m, enabled: true, alias: "" }));
  const editable = !!settings;
  const value = (m: ManagedModel): Draft =>
    drafts[keyOf(m)] ?? { enabled: m.enabled, alias: m.alias };
  const isChanged = (m: ManagedModel) => {
    const draft = drafts[keyOf(m)];
    return (
      !!draft &&
      (draft.enabled !== m.enabled || draft.alias.trim() !== m.alias.trim())
    );
  };
  const exposedID = (m: ManagedModel) => value(m).alias.trim() || m.id;
  const changed = rows.filter(isChanged);
  const usage = new Map<string, number>();
  for (const m of rows)
    if (value(m).enabled)
      usage.set(exposedID(m), (usage.get(exposedID(m)) || 0) + 1);
  const isDuplicate = (m: ManagedModel) =>
    value(m).enabled &&
    !!value(m).alias.trim() &&
    (usage.get(exposedID(m)) || 0) > 1;
  const blocked = changed.some(isDuplicate);

  useEffect(() => {
    if (!changed.length) return;
    onDirty(true);
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => {
      window.removeEventListener("beforeunload", warn);
      onDirty(false);
    };
  }, [changed.length > 0]);
  useEffect(() => {
    if (selected) dialog.current?.showModal();
    else dialog.current?.close();
  }, [selected]);
  // A modal left open inside the hidden page would block the page shown.
  useEffect(() => {
    if (!active) setSelected(null);
  }, [active]);
  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(""), 1800);
    return () => clearTimeout(timer);
  }, [copied]);

  function edit(m: ManagedModel, patch: Partial<Draft>) {
    setSaveError("");
    setDrafts((all) => ({ ...all, [keyOf(m)]: { ...value(m), ...patch } }));
  }
  function revert(m?: ManagedModel) {
    setSaveError("");
    setConflict(false);
    if (!m) return setDrafts({});
    setDrafts((all) => {
      const next = { ...all };
      delete next[keyOf(m)];
      return next;
    });
  }
  async function save() {
    if (!settings || !changed.length || blocked) return;
    setSaving(true);
    setSaveError("");
    setConflict(false);
    try {
      const next = await request<ModelSettings>("/api/model-settings", "PUT", {
        revision: settings.revision,
        models: changed.map((m) => ({
          id: m.id,
          provider: m.provider,
          enabled: value(m).enabled,
          alias: value(m).alias.trim(),
        })),
      });
      if (!next || !Array.isArray(next.models))
        throw new Error("The server returned unreadable model settings.");
      sequence.current++;
      setSettings(next);
      setDrafts({});
      notify(changed.length === 1 ? "Change saved" : "Changes saved");
      onSaved();
    } catch (err) {
      setConflict(statusOf(err) === 409);
      setSaveError(errorMessage(err, "Could not save changes."));
    } finally {
      setSaving(false);
    }
  }
  async function copyID(id: string) {
    if (await copy(id)) setCopied(id);
  }

  const providers = [...new Set(rows.map((m) => m.provider))].sort((a, b) =>
    providerName(a).localeCompare(providerName(b)),
  );
  const query = search.trim().toLowerCase();
  const filtered = rows
    .filter(
      (m) =>
        (provider === "all" || provider === m.provider) &&
        (status === "all" ||
          (status === "changed"
            ? isChanged(m)
            : value(m).enabled === (status === "enabled"))) &&
        `${m.name} ${m.id} ${value(m).alias} ${providerName(m.provider)}`
          .toLowerCase()
          .includes(query),
    )
    .sort(
      (a, b) =>
        (b.created || 0) - (a.created || 0) || a.name.localeCompare(b.name),
    );
  const detail = rows.find((m) => keyOf(m) === selected) || null;

  // The test runs in the browser, which can only call its own origin. When the
  // public address is another origin, the same server answers on this one.
  const testBase = endpoint.startsWith(`${location.origin}/`)
    ? endpoint
    : `${location.origin}/v1`;
  // Drafts are not live until saved, so a test can only use the saved state.
  const testBlock = (m: ManagedModel) =>
    isChanged(m)
      ? "Save your changes to this model first."
      : !m.enabled
        ? "Enable this model and save to test it."
        : undefined;

  return (
    <>
      {loadError && (
        <div className="notice error" role="alert">
          {loadError}
          <button disabled={loading} onClick={() => void load()}>
            Try again
          </button>
        </div>
      )}
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
      {providers.map((p) => {
        const group = filtered.filter((m) => m.provider === p);
        if (!group.length) return null;
        const all = rows.filter((m) => m.provider === p);
        const open = group.filter((m) => !m.readOnly);
        const allOn = open.every((m) => value(m).enabled);
        return (
          <section
            className="model-group"
            key={p}
            style={{ "--provider": providerColor(p) } as CSSProperties}
          >
            <div className="model-group-heading">
              <ProviderBrand provider={p} />
              <h2>{providerName(p)}</h2>
              <span>
                {all.filter((m) => value(m).enabled).length} of {all.length}{" "}
                enabled
              </span>
              {editable && open.length > 1 && (
                <button
                  className="text-button"
                  disabled={saving}
                  onClick={() => {
                    setSaveError("");
                    setDrafts((drafts) => {
                      const next = { ...drafts };
                      for (const m of open)
                        next[keyOf(m)] = { ...value(m), enabled: !allOn };
                      return next;
                    });
                  }}
                >
                  {allOn ? "Disable all" : "Enable all"}
                </button>
              )}
            </div>
            <div className="model-columns has-context" aria-hidden="true">
              <span />
              <span>Model</span>
              <span>Exposed as</span>
              <span>Context</span>
              <span />
            </div>
            <ul className="model-rows">
              {group.map((m) => {
                const current = value(m);
                const locked = !editable || saving || !!m.readOnly;
                const duplicate = isDuplicate(m);
                const exposed = exposedID(m);
                return (
                  <li
                    key={keyOf(m)}
                    className={[
                      "model-row",
                      "has-context",
                      current.enabled ? "" : "is-off",
                      isChanged(m) ? "is-changed" : "",
                    ].join(" ")}
                  >
                    <button
                      className="switch"
                      role="switch"
                      aria-checked={current.enabled}
                      aria-label={`Expose ${m.id}`}
                      title={m.readOnly}
                      disabled={locked}
                      onClick={() => edit(m, { enabled: !current.enabled })}
                    />
                    <div className="model-name">
                      <button onClick={() => setSelected(keyOf(m))}>
                        {m.name}
                      </button>
                      {m.readOnly && <p>{m.readOnly}</p>}
                    </div>
                    <div className="model-alias">
                      <div className="model-slug">
                        <input
                          aria-label={`Exposed model ID for ${m.id}`}
                          aria-invalid={duplicate}
                          value={current.alias}
                          placeholder={m.id}
                          disabled={locked}
                          spellCheck={false}
                          autoComplete="off"
                          autoCapitalize="off"
                          onChange={(e) => edit(m, { alias: e.target.value })}
                        />
                        <button
                          className="icon-button"
                          aria-label={`Copy slug ${exposed}`}
                          title="Copy slug"
                          onClick={() => void copyID(exposed)}
                        >
                          {copied === exposed ? (
                            <Check size={14} />
                          ) : (
                            <Copy size={14} />
                          )}
                        </button>
                      </div>
                      {duplicate && (
                        <p role="alert">Another enabled model uses this ID.</p>
                      )}
                    </div>
                    <div
                      className="model-context"
                      title={
                        m.context
                          ? `${m.context.toLocaleString()} tokens`
                          : "Provider context unknown"
                      }
                    >
                      {m.context ? compact(m.context) : "Unknown"}
                    </div>
                    <div className="model-actions">
                      {isChanged(m) && (
                        <button
                          className="icon-button"
                          aria-label={`Revert ${m.id}`}
                          title="Revert"
                          disabled={saving}
                          onClick={() => revert(m)}
                        >
                          <Undo2 size={14} />
                        </button>
                      )}
                    </div>
                  </li>
                );
              })}
            </ul>
          </section>
        );
      })}
      {!filtered.length &&
        (loading && !rows.length ? (
          <div className="loading" role="status" aria-label="Loading">
            <RefreshCw size={22} className="spinning" />
          </div>
        ) : (
          <div className="empty">
            <p>{rows.length ? "No matching models" : "No models yet"}</p>
            {rows.length ? (
              <button
                className="secondary"
                onClick={() => {
                  setSearch("");
                  setProvider("all");
                  setStatus("all");
                }}
              >
                Clear filters
              </button>
            ) : (
              <button className="secondary" onClick={onAddAccount}>
                Add account
              </button>
            )}
          </div>
        ))}
      {changed.length > 0 && (
        <div className="save-bar" role="region" aria-label="Unsaved changes">
          <p>
            <strong>
              {changed.length} unsaved{" "}
              {changed.length === 1 ? "change" : "changes"}
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
            <button
              className="secondary"
              disabled={loading}
              onClick={() => void load()}
            >
              Reload settings
            </button>
          )}
          <button
            className="secondary"
            disabled={saving}
            onClick={() => revert()}
          >
            Revert
          </button>
          <button
            className="primary"
            disabled={saving || blocked || conflict}
            onClick={() => void save()}
          >
            {saving ? "Saving" : "Save changes"}
          </button>
        </div>
      )}
      <dialog
        ref={dialog}
        onCancel={() => setSelected(null)}
        onClick={(e) => {
          if (isDialogBackdropClick(e)) setSelected(null);
        }}
        aria-labelledby="model-dialog-title"
      >
        {detail && (
          <>
            <div className="dialog-heading">
              <h2 id="model-dialog-title">{detail.name}</h2>
              <button
                className="icon-button"
                aria-label="Close model details"
                onClick={() => setSelected(null)}
              >
                <X size={18} />
              </button>
            </div>
            <div className="model-id">
              <code>{exposedID(detail)}</code>
              <button
                className="icon-button"
                aria-label="Copy model ID"
                onClick={() => void copyID(exposedID(detail))}
              >
                {copied === exposedID(detail) ? (
                  <Check size={15} />
                ) : (
                  <Copy size={15} />
                )}
              </button>
            </div>
            <dl className="model-facts">
              {exposedID(detail) !== detail.id && (
                <div>
                  <dt>Original ID</dt>
                  <dd>
                    <code>{detail.id}</code>
                  </dd>
                </div>
              )}
              {detail.context > 0 && (
                <div>
                  <dt>Context</dt>
                  <dd>{detail.context.toLocaleString()} tokens</dd>
                </div>
              )}
              {!!detail.maxOutput && (
                <div>
                  <dt>Max output</dt>
                  <dd>{detail.maxOutput.toLocaleString()} tokens</dd>
                </div>
              )}
              {!!detail.inputs?.length && (
                <div>
                  <dt>Input</dt>
                  <dd>{detail.inputs.join(", ")}</dd>
                </div>
              )}
              {detail.reasoningSupported && (
                <div>
                  <dt>Reasoning</dt>
                  <dd>
                    {detail.reasoning?.length
                      ? detail.reasoning.join(", ")
                      : "Supported"}
                  </dd>
                </div>
              )}
            </dl>
            <div className="protocol-choice">
              <span>Client protocol</span>
              <Select
                label="Client protocol"
                value={protocol}
                options={protocols}
                onChange={setProtocol}
              />
            </div>
            <CodeBlock
              code={curlExample(endpoint, protocol, exposedID(detail))}
              method="POST"
              path={`/v1${protocolPath(protocol)}`}
              copy={copy}
            />
            <ConnectionTest
              base={testBase}
              protocol={protocol}
              model={exposedID(detail)}
              provider={detail.provider}
              blocked={testBlock(detail)}
            />
          </>
        )}
      </dialog>
    </>
  );
}
