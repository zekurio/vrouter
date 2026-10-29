import { Check, Copy, Undo2 } from "lucide-react";
import { ProviderBrand, providerName, providerStyle } from "./ProviderBrand";
import { keyOf, type Draft, type DraftView, type ManagedModel } from "./models";
import {
  cx,
  groupCount,
  groupHeading,
  groupTitle,
  iconButton,
  panelFrame,
  textButton,
  toggle,
} from "./ui";

// Switch, name, exposed ID, context and actions. Below 720px the row stacks:
// switch, name and actions on top, then the ID, then the context.
const columns =
  "grid grid-cols-[30px_minmax(0,1fr)_minmax(0,1fr)_72px_28px] items-center gap-x-4";

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
  first,
  provider: p,
  group,
  all,
  rows,
  onSetEnabled,
}: {
  // False for every group but the top one, which sit a gap apart.
  first: boolean;
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
    <section
      className={cx(
        panelFrame,
        "px-5.5 pt-5 pb-1.5 max-sm:p-4.5",
        !first && "mt-3",
      )}
      style={providerStyle(p)}
    >
      <div
        className={cx(
          groupHeading,
          "mb-3.5 max-md:mb-0 max-md:border-b max-md:border-border max-md:pb-3.5",
        )}
      >
        <ProviderBrand provider={p} />
        <h2 className={groupTitle}>{providerName(p)}</h2>
        <span className={groupCount}>
          {all.filter((m) => value(m).enabled).length} of {all.length} enabled
        </span>
        {rows.editable && open.length > 1 && (
          <button
            className={cx(textButton, "ml-auto")}
            disabled={rows.saving}
            onClick={() => onSetEnabled(open, !allOn)}
          >
            {allOn ? "Disable all" : "Enable all"}
          </button>
        )}
      </div>
      <div
        className={cx(
          columns,
          "border-b border-border pb-2.5 text-[12px] text-muted max-md:hidden",
        )}
        aria-hidden="true"
      >
        <span />
        <span>Model</span>
        <span className="pl-2.5">Exposed as</span>
        <span>Context</span>
        <span />
      </div>
      <ul>
        {group.map((m) => (
          <ModelRow key={keyOf(m)} model={m} rows={rows} />
        ))}
      </ul>
    </section>
  );
}

// The exposed ID field's border and fill. Hovering shows the border, an ID
// in use elsewhere turns it red, and an empty field is borderless until it
// has focus, except on narrow screens.
function aliasState(duplicate: boolean, filled: boolean) {
  if (duplicate)
    return cx("border-danger", filled ? "bg-well" : "focus:bg-well");
  if (filled) return "border-line bg-well";
  return "border-transparent focus:border-accent focus:bg-well max-md:border-border";
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
  const off = !current.enabled && "opacity-50";
  return (
    <li
      className={cx(
        columns,
        "relative min-h-13 border-b border-border py-2 last:border-b-0 max-md:grid-cols-[30px_minmax(0,1fr)_auto] max-md:gap-y-2 max-md:py-3",
        // A changed row is marked in the panel's margin.
        view.isChanged(m) &&
          "before:absolute before:top-2.25 before:bottom-2.25 before:-left-3.5 before:w-0.5 before:rounded-[1px] before:bg-accent max-md:before:-left-2.5",
      )}
    >
      <button
        className={toggle}
        role="switch"
        aria-checked={current.enabled}
        aria-label={`Expose ${m.id}`}
        title={m.readOnly}
        disabled={locked}
        onClick={() => onEdit(m, { enabled: !current.enabled })}
      />
      <div
        className={cx(
          "flex min-w-0 flex-col items-start gap-1 max-md:col-start-2 max-md:row-start-1",
          off,
        )}
      >
        <button
          className="text-left text-[14px] font-medium wrap-anywhere underline-offset-3 hover:underline"
          onClick={() => rows.onSelect(keyOf(m))}
        >
          {m.name}
        </button>
        {readOnly && (
          <p className="text-[11px] leading-normal text-muted">{readOnly}</p>
        )}
      </div>
      <div className="max-md:col-start-2 max-md:col-end-4 max-md:row-start-2">
        <div className="flex min-w-0 items-center">
          <input
            className={cx(
              "h-8 w-full min-w-0 rounded-lg border px-2.25 font-mono text-[12px] text-ellipsis text-text placeholder:text-muted focus:outline-none hover:not-disabled:border-line",
              aliasState(duplicate, current.alias !== ""),
              off,
            )}
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
            className={iconButton()}
            aria-label={`Copy slug ${exposed}`}
            title="Copy slug"
            onClick={() => rows.onCopyID(exposed)}
          >
            {copied === exposed ? <Check size={14} /> : <Copy size={14} />}
          </button>
        </div>
        {duplicate && (
          <p
            className="mt-1.25 ml-2.5 text-[11px] leading-normal text-danger max-md:ml-0"
            role="alert"
          >
            Another enabled model uses this ID.
          </p>
        )}
      </div>
      <div
        className={cx(
          "text-[13px] text-subtle tabular-nums max-md:col-start-2 max-md:col-end-4 max-md:row-start-3",
          off,
        )}
        title={
          m.context
            ? `${m.context.toLocaleString()} tokens`
            : "Provider context unknown"
        }
      >
        {m.context ? compact(m.context) : "Unknown"}
      </div>
      <div className="flex justify-end max-md:col-start-3 max-md:row-start-1">
        {view.isChanged(m) && (
          <button
            className={iconButton()}
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
