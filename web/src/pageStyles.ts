// Classes the Keys, Users and Usage pages share: their toolbars, the usage
// panels, and the key table, which the users list reuses.
import { cx, panelFrame } from "./ui";

// Mine and Everyone, above an admin's keys and usage, with a note beside.
export const pageToolbar =
  "mb-4.5 flex flex-wrap items-center gap-x-4 gap-y-2.5";
export const pageNote = "text-[13px] leading-[1.5] text-muted";

// A section's heading or filters and the controls at its end. Callers set the
// margin.
export const sectionToolbar =
  "flex flex-wrap items-center justify-between gap-x-5 gap-y-3";
export const sectionHeading = "text-[15px] font-semibold";
export const caption = "text-[13px] leading-[1.6] text-muted";
// Each usage panel after the first sits 12px below the one before. Callers
// set the padding.
export const nextPanel = `${panelFrame} mt-3`;

export const keyTable = "w-full min-w-[820px] border-collapse text-[13px]";
type Align = "left" | "right" | "numeric";
const align: Record<Align, string> = {
  left: "text-left",
  right: "text-right",
  numeric: "text-right tabular-nums",
};
export const keyHead = (a: Align = "left") =>
  cx(
    "border-b border-border pr-4.5 pb-2.75 text-[12px] font-[450] whitespace-nowrap text-muted last:pr-0",
    align[a],
  );
// Each key has a tbody of its own; the users share one. Either way a tbody
// is ruled off from the next.
export const keyBody = "border-b border-border last:border-b-0";
// Rows that no longer work fade, except their status and actions.
export const faded = "opacity-45";
export const keyRowHead = (off = false) =>
  cx(
    "py-3 pr-4.5 text-left text-[14px] font-[550] text-text wrap-anywhere last:pr-0",
    off && faded,
  );
export const keyCell = (a: Align = "left", off = false) =>
  cx(
    "py-3 pr-4.5 whitespace-nowrap text-subtle last:pr-0",
    align[a],
    off && faded,
  );

const statusTones = {
  active: "text-text before:bg-green",
  revoked: "text-danger before:bg-danger",
  expired: "text-amber before:bg-amber",
};
// A status cell, led by a dot in its colour.
export const keyStatus = (state: keyof typeof statusTones) =>
  cx(
    "py-3 pr-4.5 text-left whitespace-nowrap last:pr-0 before:mr-2 before:mb-px before:inline-block before:size-1.75 before:rounded-full",
    statusTones[state],
  );
// The edit, revoke and delete buttons at the end of a row.
export const rowActions = "flex justify-end gap-1.5";
