import { Check, Copy, Undo2 } from "lucide-react";
import { ProviderBrand, providerName, providerStyle } from "./ProviderBrand";
import { keyOf, type Draft, type DraftView, type ManagedModel } from "./models";

const compact = (n: number) =>
  Intl.NumberFormat("en", {
    notation: "compact",
    maximumFractionDigits: 1,
  }).format(n);

// What every model row reads and can do.
export type RowContext = {
  view: DraftView;
  // False until the saved settings have loaded.
  editable: boolean;
  saving: boolean;
  // The ID copied last, shown with a check mark for a moment.
  copied: string;
  onCopyID: (id: string) => void;
  onSelect: (key: string) => void;
  onEdit: (m: ManagedModel, patch: Partial<Draft>) => void;
  onRevert: (m: ManagedModel) => void;
};

// One provider's models that match the filters.
export function ModelGroup({
  provider: p,
  group,
  all,
  rows,
  onSetEnabled,
}: {
  provider: string;
  group: ManagedModel[];
  // Every model of the provider, filtered or not.
  all: ManagedModel[];
  rows: RowContext;
  onSetEnabled: (models: ManagedModel[], enabled: boolean) => void;
}) {
  const { value } = rows.view;
  const open = group.filter((m) => (m.readOnly ?? "") === "");
  const allOn = open.every((m) => value(m).enabled);
  return (
    <section className="model-group" style={providerStyle(p)}>
      <div className="model-group-heading">
        <ProviderBrand provider={p} />
        <h2>{providerName(p)}</h2>
        <span>
          {all.filter((m) => value(m).enabled).length} of {all.length} enabled
        </span>
        {rows.editable && open.length > 1 && (
          <button
            className="text-button"
            disabled={rows.saving}
            onClick={() => onSetEnabled(open, !allOn)}
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
        {group.map((m) => (
          <ModelRow key={keyOf(m)} model={m} rows={rows} />
        ))}
      </ul>
    </section>
  );
}

function ModelRow({
  model: m,
  rows,
}: {
  model: ManagedModel;
  rows: RowContext;
}) {
  const { view, saving, copied, onEdit } = rows;
  const current = view.value(m);
  const readOnly = m.readOnly ?? "";
  const locked = !rows.editable || saving || !!readOnly;
  const duplicate = view.isDuplicate(m);
  const exposed = view.exposedID(m);
  return (
    <li
      className={[
        "model-row",
        "has-context",
        current.enabled ? "" : "is-off",
        view.isChanged(m) ? "is-changed" : "",
      ].join(" ")}
    >
      <button
        className="switch"
        role="switch"
        aria-checked={current.enabled}
        aria-label={`Expose ${m.id}`}
        title={m.readOnly}
        disabled={locked}
        onClick={() => onEdit(m, { enabled: !current.enabled })}
      />
      <div className="model-name">
        <button onClick={() => rows.onSelect(keyOf(m))}>{m.name}</button>
        {readOnly && <p>{readOnly}</p>}
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
            onChange={(e) => onEdit(m, { alias: e.target.value })}
          />
          <button
            className="icon-button"
            aria-label={`Copy slug ${exposed}`}
            title="Copy slug"
            onClick={() => rows.onCopyID(exposed)}
          >
            {copied === exposed ? <Check size={14} /> : <Copy size={14} />}
          </button>
        </div>
        {duplicate && <p role="alert">Another enabled model uses this ID.</p>}
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
        {view.isChanged(m) && (
          <button
            className="icon-button"
            aria-label={`Revert ${m.id}`}
            title="Revert"
            disabled={saving}
            onClick={() => rows.onRevert(m)}
          >
            <Undo2 size={14} />
          </button>
        )}
      </div>
    </li>
  );
}
