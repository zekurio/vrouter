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

// Below 800px the table becomes a list, because its columns do not fit a
// phone or an upright tablet and the actions would scroll out of reach. Each
// row is a block: the name on top, the actions at its top right, and the
// other cells in two columns under the labels their data-label names.
export const keyTable =
  "w-full min-w-[820px] border-collapse text-[13px] max-lg:block max-lg:min-w-0";
// The column headings. A list reads its labels from the cells.
export const keyHeader = "max-lg:sr-only";
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
// is ruled off from the next. In a list the rows carry the rule.
export const keyBody =
  "border-b border-border last:border-b-0 max-lg:block max-lg:border-b-0";
export const keyRow =
  "max-lg:relative max-lg:grid max-lg:grid-cols-3 max-lg:gap-x-4 max-sm:grid-cols-2 max-lg:border-b max-lg:border-border max-lg:py-2.5 max-lg:[tbody:last-child>&:last-child]:border-b-0";
// Rows that no longer work fade, except their status and actions.
export const faded = "opacity-45";
// The name. In a list it leaves room for the actions beside it.
export const keyRowHead = (off = false) =>
  cx(
    "py-3 pr-4.5 text-left text-[14px] font-[550] text-text wrap-anywhere last:pr-0 max-lg:col-span-full max-lg:block max-lg:min-h-10 max-lg:py-1.5 max-lg:pr-36 max-lg:wrap-break-word",
    off && faded,
  );
const listCell =
  "max-lg:block max-lg:py-1.5 max-lg:pr-0 max-lg:text-left max-lg:whitespace-normal max-lg:before:block max-lg:before:text-[12px] max-lg:before:text-muted max-lg:before:content-[attr(data-label)]";
export const keyCell = (a: Align = "left", off = false) =>
  cx(
    "py-3 pr-4.5 whitespace-nowrap text-subtle last:pr-0",
    align[a],
    listCell,
    off && faded,
  );

const statusTones = {
  active: "text-text before:bg-green",
  revoked: "text-danger before:bg-danger",
  expired: "text-amber before:bg-amber",
};
// A status, led by a dot in its colour.
export const keyStatus = (state: keyof typeof statusTones) =>
  cx(
    "whitespace-nowrap before:mr-2 before:mb-px before:inline-block before:size-1.75 before:rounded-full",
    statusTones[state],
  );
// The cell of the buttons at the end of a row.
export const keyActions =
  "py-3 text-right whitespace-nowrap max-lg:absolute max-lg:top-1.5 max-lg:right-0 max-lg:py-0";
// The edit, revoke and delete buttons in it.
export const rowActions = "flex justify-end gap-1.5";
