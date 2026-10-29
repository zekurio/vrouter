import { tab, tabs } from "./ui";

// Whose records an admin's page lists: their own, or everyone's.
export type Owner = "mine" | "all";
const owners: { value: Owner; label: string }[] = [
  { value: "mine", label: "Mine" },
  { value: "all", label: "Everyone" },
];

// The query an owner adds to a list request.
export const ownerQuery = (owner: Owner) =>
  owner === "all" ? "owner=all" : "";

export function OwnerTabs({
  owner,
  setOwner,
  label,
}: {
  owner: Owner;
  setOwner: (owner: Owner) => void;
  label: string;
}) {
  return (
    <div className={tabs()} role="group" aria-label={label}>
      {owners.map((o) => (
        <button
          key={o.value}
          aria-pressed={owner === o.value}
          className={tab(owner === o.value)}
          onClick={() => setOwner(o.value)}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}
