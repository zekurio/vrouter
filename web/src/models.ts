// Model settings as the models page edits them: saved state plus local drafts.

import { providerName } from "./ProviderBrand";

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
export type ManagedModel = Model & {
  enabled: boolean;
  alias: string;
  readOnly?: string;
};
export type ModelSettings = { models: ManagedModel[]; revision: string };
export type Draft = { enabled: boolean; alias: string };
export type Status = "all" | "enabled" | "disabled" | "changed";

export const keyOf = (m: { provider: string; id: string }) =>
  `${m.provider}\n${m.id}`;

// Each model as drafted, and which drafts differ from what is saved.
export function draftView(rows: ManagedModel[], drafts: Record<string, Draft>) {
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
  const changed = rows.filter((m) => isChanged(m));
  const usage = new Map<string, number>();
  for (const m of rows)
    if (value(m).enabled)
      usage.set(exposedID(m), (usage.get(exposedID(m)) ?? 0) + 1);
  const isDuplicate = (m: ManagedModel) =>
    value(m).enabled &&
    !!value(m).alias.trim() &&
    (usage.get(exposedID(m)) ?? 0) > 1;
  const blocked = changed.some((m) => isDuplicate(m));
  return { value, isChanged, exposedID, changed, isDuplicate, blocked };
}
export type DraftView = ReturnType<typeof draftView>;

// The models that match the search and filters, newest first.
export function filterModels(
  rows: ManagedModel[],
  view: DraftView,
  filter: { search: string; provider: string; status: Status },
) {
  const { value, isChanged } = view;
  const { provider, status } = filter;
  const query = filter.search.trim().toLowerCase();
  return rows
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
    .toSorted(
      (a, b) =>
        (b.created ?? 0) - (a.created ?? 0) || a.name.localeCompare(b.name),
    );
}
