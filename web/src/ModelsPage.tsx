import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type CSSProperties,
} from "react";
import { Check, Copy, RefreshCw, Search, Undo2, X } from "lucide-react";
import { ProviderBrand, providerColor, providerName } from "./ProviderBrand";
import type { APIRequest } from "./AccountsPage";
import { isDialogBackdropClick } from "./dialog";
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
  defaultContext?: number;
  contextOverride?: number;
};
type ModelSettings = { models: ManagedModel[]; revision: string };
type Draft = { enabled: boolean; alias: string; context: string };
type Status = "all" | "enabled" | "disabled" | "changed";
const statuses: SelectOption<Status>[] = [
  { value: "all", label: "All models" },
  { value: "enabled", label: "Enabled" },
  { value: "disabled", label: "Disabled" },
  { value: "changed", label: "Unsaved" },
];
// vrouter forwards each provider in its own protocol and does not translate.
const isClaude = (provider: string) => provider === "Claude";
const compact = (n: number) =>
  Intl.NumberFormat("en", {
    notation: "compact",
    maximumFractionDigits: 1,
  }).format(n);
const shellQuote = (value: string) => `'${value.replaceAll("'", "'\\''")}'`;
const keyOf = (m: { provider: string; id: string }) => `${m.provider}\n${m.id}`;
const message = (err: unknown, fallback: string) =>
  err instanceof Error ? err.message : fallback;

export function ModelsPage({
  models,
  request,
  live,
  reloadKey,
  endpoint,
  copy,
  notify,
  onSaved,
  onAddAccount,
}: {
  models: Model[];
  request: APIRequest;
  live: boolean;
  reloadKey: string;
  endpoint: string;
  copy: (value: string) => Promise<boolean>;
  notify: (text: string) => void;
  onSaved: () => void;
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
      if (current !== sequence.current) return;
      setLoadError(message(err, "Could not load model settings."));
    } finally {
      if (current === sequence.current) setLoading(false);
    }
  }, [request]);
  useEffect(() => {
    if (live) void load();
    else {
      sequence.current++;
      setSettings(null);
      setLoadError("");
      setLoading(false);
    }
  }, [live, reloadKey, load]);

  const rows: ManagedModel[] =
    settings?.models ?? models.map((m) => ({ ...m, enabled: true, alias: "" }));
  const editable = live && !!settings;
  const value = (m: ManagedModel): Draft =>
    drafts[keyOf(m)] ?? {
      enabled: m.enabled,
      alias: m.alias,
      context: m.contextOverride ? String(m.contextOverride) : "",
    };
  const defaultContext = (m: ManagedModel) => m.defaultContext ?? m.context;
  const contextError = (m: ManagedModel) => {
    const input = value(m).context.trim();
    return (
      input !== "" &&
      (!/^\d+$/.test(input) || Number(input) < 1 || Number(input) > 2147483647)
    );
  };
  const effectiveContext = (m: ManagedModel) =>
    contextError(m) ? 0 : Number(value(m).context.trim()) || defaultContext(m);
  const isChanged = (m: ManagedModel) => {
    const draft = drafts[keyOf(m)];
    return (
      !!draft &&
      (draft.enabled !== m.enabled ||
        draft.alias.trim() !== m.alias.trim() ||
        contextError(m) ||
        Number(draft.context.trim()) !== (m.contextOverride || 0))
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
  const invalidContext = changed.some(contextError);
  const blocked = changed.some(isDuplicate) || invalidContext;

  useEffect(() => {
    if (!changed.length) return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [changed.length > 0]);
  useEffect(() => {
    if (selected) dialog.current?.showModal();
    else dialog.current?.close();
  }, [selected]);
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
          context: Number(value(m).context.trim()),
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
      setConflict((err as { status?: number }).status === 409);
      setSaveError(message(err, "Could not save changes."));
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

  function example(m: ManagedModel) {
    const id = exposedID(m);
    const claude = isClaude(m.provider);
    const path = claude ? "/messages" : "/responses";
    const body = claude
      ? {
          model: id,
          max_tokens: 1024,
          messages: [{ role: "user", content: "Hello" }],
          stream: true,
        }
      : {
          model: id,
          input: [{ role: "user", content: "Hello" }],
          stream: true,
          store: false,
        };
    const auth = claude
      ? '-H "x-api-key: $VROUTER_API_KEY" \\\n  -H "anthropic-version: 2023-06-01"'
      : '-H "Authorization: Bearer $VROUTER_API_KEY"';
    return `curl --no-buffer ${shellQuote(endpoint + path)} \\\n  ${auth} \\\n  -H "Content-Type: application/json" \\\n  -d ${shellQuote(JSON.stringify(body, null, 2))}`;
  }

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
      {!live && rows.length > 0 && (
        <div className="notice">Model settings are read-only in demo mode.</div>
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
      {rows.length > 0 && (
        <p className="models-help" id="context-help">
          Context is in tokens. Leave blank to use the provider value. Overrides
          change the advertised limit; provider limits still apply.
        </p>
      )}
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
                    <div className="model-context">
                      <input
                        aria-label={`Context window for ${m.id}`}
                        aria-describedby="context-help"
                        aria-invalid={contextError(m)}
                        inputMode="numeric"
                        value={current.context}
                        placeholder={
                          defaultContext(m)
                            ? `${compact(defaultContext(m))} auto`
                            : "Unknown"
                        }
                        title={
                          defaultContext(m)
                            ? `Provider value: ${defaultContext(m).toLocaleString()} tokens. Clear to restore.`
                            : "Provider context unknown. Enter a limit in tokens."
                        }
                        disabled={locked}
                        onChange={(e) => edit(m, { context: e.target.value })}
                      />
                      {contextError(m) && (
                        <p role="alert">
                          Enter a whole number from 1 to 2,147,483,647.
                        </p>
                      )}
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
      {!filtered.length && (
        <div className="empty">
          {loading && !rows.length ? (
            <RefreshCw size={22} className="spinning" />
          ) : (
            <>
              <h2>{rows.length ? "No matching models" : "No models yet"}</h2>
              {rows.length === 0 && (
                <>
                  <p>Models appear once an account is connected.</p>
                  <button
                    className="secondary"
                    disabled={!live}
                    onClick={onAddAccount}
                  >
                    Add account
                  </button>
                </>
              )}
              {rows.length > 0 && (
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
              )}
            </>
          )}
        </div>
      )}
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
                <span className="save-error">
                  {invalidContext
                    ? "Fix invalid context windows first."
                    : "Resolve duplicate IDs first."}
                </span>
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
              {effectiveContext(detail) > 0 && (
                <div>
                  <dt>Context</dt>
                  <dd>{effectiveContext(detail).toLocaleString()} tokens</dd>
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
            <div className="client-format">
              <code>
                POST{" "}
                {isClaude(detail.provider) ? "/v1/messages" : "/v1/responses"}
              </code>
              <button
                className="secondary"
                onClick={() => void copy(example(detail))}
              >
                <Copy size={13} />
                Copy request
              </button>
            </div>
            <pre className="snippet">{example(detail)}</pre>
          </>
        )}
      </dialog>
    </>
  );
}
