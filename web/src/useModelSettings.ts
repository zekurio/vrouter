import { useCallback, useEffect, useRef, useState } from "react";
import { errorMessage, isStale, statusOf, type APIRequest } from "./api";
import {
  draftView,
  keyOf,
  type Draft,
  type ManagedModel,
  type Model,
  type ModelSettings,
} from "./models";

// The saved model settings and the drafts made on top of them.
export function useModelSettings({
  models,
  request,
  reloadKey,
  notify,
  onSaved,
}: {
  models: Model[];
  request: APIRequest;
  reloadKey: string;
  notify: (text: string) => void;
  onSaved: () => void;
}) {
  const [settings, setSettings] = useState<ModelSettings | null>(null);
  const [loading, setLoading] = useState(false);
  const [loadError, setLoadError] = useState("");
  const [drafts, setDrafts] = useState<Record<string, Draft>>({});
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [conflict, setConflict] = useState(false);
  const sequence = useRef(0);

  const load = useCallback(async () => {
    const current = ++sequence.current;
    setLoading(true);
    try {
      const next = await request<ModelSettings | null>("/api/model-settings");
      if (current !== sequence.current) return;
      if (next === null || !Array.isArray(next.models))
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
  const view = draftView(rows, drafts);
  const { value, changed, blocked } = view;

  function edit(m: ManagedModel, patch: Partial<Draft>) {
    setSaveError("");
    setDrafts((all) => ({ ...all, [keyOf(m)]: { ...value(m), ...patch } }));
  }
  function setEnabled(list: ManagedModel[], enabled: boolean) {
    setSaveError("");
    setDrafts((previous) => {
      const next = { ...previous };
      for (const m of list) next[keyOf(m)] = { ...value(m), enabled };
      return next;
    });
  }
  function revert(m?: ManagedModel) {
    setSaveError("");
    setConflict(false);
    if (!m) {
      setDrafts({});
      return;
    }
    setDrafts((all) => {
      const next = { ...all };
      delete next[keyOf(m)];
      return next;
    });
  }
  async function save() {
    if (!settings || changed.length === 0 || blocked) return;
    setSaving(true);
    setSaveError("");
    setConflict(false);
    try {
      const next = await request<ModelSettings | null>(
        "/api/model-settings",
        "PUT",
        {
          revision: settings.revision,
          models: changed.map((m) => ({
            id: m.id,
            provider: m.provider,
            enabled: value(m).enabled,
            alias: value(m).alias.trim(),
          })),
        },
      );
      if (next === null || !Array.isArray(next.models))
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

  return {
    rows,
    view,
    editable: !!settings,
    loading,
    loadError,
    saving,
    saveError,
    conflict,
    load,
    edit,
    setEnabled,
    revert,
    save,
  };
}

// Reports unsaved drafts and warns before the page unloads while there are
// some. Runs when the page becomes dirty or clean, not on every new onDirty.
export function useDirtyReport(
  dirty: boolean,
  onDirty: (dirty: boolean) => void,
) {
  const onDirtyRef = useRef(onDirty);
  onDirtyRef.current = onDirty;
  useEffect(() => {
    if (!dirty) return undefined;
    const report = onDirtyRef.current;
    report(true);
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => {
      window.removeEventListener("beforeunload", warn);
      report(false);
    };
  }, [dirty]);
}
