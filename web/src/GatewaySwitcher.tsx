import type { Gateway } from "./api";
import { Select } from "./Select";

export function GatewaySwitcher({
  gateways,
  selected,
  onSelect,
}: {
  gateways: Gateway[];
  selected: string | null;
  onSelect: (id: string) => void;
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
    </div>
  );
}
