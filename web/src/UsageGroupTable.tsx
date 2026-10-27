import { series } from "./CostChart";
import { Private } from "./Privacy";
import { ProviderBrand } from "./ProviderBrand";
import { compact, dayLabel, money, share, type Group } from "./usage";

// What a breakdown table groups the usage by.
export type GroupView = "model" | "key" | "scope" | "user" | "day";
const groupHeading: Record<GroupView, string> = {
  model: "Model",
  key: "Key",
  user: "User",
  scope: "Account type",
  day: "Day",
};

const number = (n: number) => n.toLocaleString();

// One breakdown of the usage, largest cost first, with a bar for each row.
export function GroupTable({
  view,
  groups,
  total,
}: {
  view: GroupView;
  groups: Group[];
  total: number;
}) {
  const peak = Math.max(...groups.map((g) => g.cost), 0);
  return (
    <div className="table-scroll">
      <table className="usage-table">
        <thead>
          <tr>
            <th scope="col">{groupHeading[view]}</th>
            <th scope="col" className="numeric">
              Cost
            </th>
            <th scope="col" className="numeric">
              Share
            </th>
            <th scope="col" className="numeric">
              Requests
            </th>
            <th scope="col" className="numeric">
              Tokens
            </th>
          </tr>
        </thead>
        <tbody>
          {groups.map((g) => (
            <tr key={`${g.provider}/${g.name}`} style={series(g.provider)}>
              <th scope="row">
                <span className="usage-name">
                  {view === "model" && (
                    <ProviderBrand provider={g.provider} small />
                  )}
                  {view === "day" ? (
                    dayLabel(Number(g.name))
                  ) : view === "user" ? (
                    <Private peek>{g.name}</Private>
                  ) : (
                    g.name
                  )}
                </span>
                {peak > 0 && g.cost > 0 && (
                  <span
                    className="usage-bar"
                    style={{ width: `${(g.cost / peak) * 100}%` }}
                  />
                )}
              </th>
              {g.measured > 0 && g.unpriced === g.measured ? (
                <td className="numeric unpriced" colSpan={2}>
                  Unpriced
                </td>
              ) : (
                <>
                  <td className="numeric cost">
                    {g.measured > 0 && money(g.cost)}
                  </td>
                  <td className="numeric">{share(g.cost, total)}</td>
                </>
              )}
              <td className="numeric">{number(g.requests)}</td>
              <td className="numeric">{g.measured > 0 && compact(g.tokens)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
