import { useCallback, useEffect, useState } from "react";
import { errorMessage, isStale, type APIRequest } from "./api";
import { Select, type SelectOption } from "./Select";
import { useToast } from "./Toaster";
import { cx, noticeAction, panel } from "./ui";

type Routing = "personal-first" | "global-first";
const routings: SelectOption<Routing>[] = [
  { value: "personal-first", label: "Personal accounts first" },
  { value: "global-first", label: "Shared accounts first" },
];
const routingNote: Record<Routing, string> = {
  "personal-first":
    "Requests use the key owner's personal accounts and fall back to shared ones when none can serve the model.",
  "global-first":
    "Requests use shared accounts and fall back to the key owner's personal accounts when none can serve the model.",
};

const note = "mt-1.25 max-w-[720px] leading-normal";

// The account tier requests try first. Admins choose it for everyone.
export function RoutingPanel({
  admin,
  request,
  notify,
}: {
  admin: boolean;
  request: APIRequest;
  notify: (message: string) => void;
}) {
  const [routing, setRouting] = useState<Routing | null>(null);
  // Why the order could not be loaded.
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const toast = useToast();
  const load = useCallback(async () => {
    try {
      const result = await request<{ routing: Routing }>("/api/settings");
      setRouting(result.routing);
      setError("");
    } catch (err) {
      if (!isStale(err))
        setError(errorMessage(err, "Could not load the routing order."));
    }
  }, [request]);
  useEffect(() => {
    void load();
  }, [load]);

  async function change(next: Routing) {
    setSaving(true);
    try {
      const result = await request<{ routing: Routing }>(
        "/api/settings",
        "PUT",
        { routing: next },
      );
      setRouting(result.routing);
      notify("Routing order saved");
    } catch (err) {
      // The select keeps the saved order, so the failure is all there is to
      // show.
      toast(errorMessage(err, "Could not save the routing order."), {
        failed: true,
        id: "routing",
        action: { label: "Retry", run: () => void change(next) },
      });
    } finally {
      setSaving(false);
    }
  }

  return (
    <section
      className={cx(
        panel,
        "flex flex-wrap items-center justify-between gap-x-6 gap-y-3.5",
      )}
      aria-labelledby="routing-title"
    >
      <div>
        <h2 id="routing-title" className="text-[15px] font-semibold">
          Routing order
        </h2>
        {routing || !error ? (
          <p className={cx(note, "text-muted")}>
            {routing ? routingNote[routing] : "Loading the routing order."}
          </p>
        ) : (
          <p className={cx(note, "text-danger wrap-anywhere")} role="alert">
            {error}
            <button className={noticeAction} onClick={() => void load()}>
              Try again
            </button>
          </p>
        )}
      </div>
      {routing &&
        (admin ? (
          <div className={saving ? "pointer-events-none opacity-60" : ""}>
            <Select
              inset
              label="Routing order"
              value={routing}
              options={routings}
              onChange={(next) => void change(next)}
            />
          </div>
        ) : (
          <span className="inline-block text-[12px] font-medium text-text">
            {routings.find((r) => r.value === routing)?.label}
          </span>
        ))}
    </section>
  );
}
