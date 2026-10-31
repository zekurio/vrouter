// Tailwind classes for the building blocks several components share. Each
// helper returns every class its element needs. Callers add only classes
// the helper leaves unset, such as margins or grid placement, because two
// utilities for one property apply in stylesheet order, not class order.

export const cx = (...parts: (string | false | null | undefined)[]) =>
  parts.filter(Boolean).join(" ");

// Panels group a page's content. The light theme rings them, so they stand
// off the grey page. panelFrame leaves the padding to the caller.
export const panelFrame =
  "rounded-[14px] bg-surface light:ring-1 light:ring-border max-sm:rounded-xl";
export const panel = `${panelFrame} px-5.5 py-5 max-sm:p-4.5`;

type ButtonVariant = "primary" | "secondary" | "danger" | "danger-text";
const buttonVariants: Record<ButtonVariant, string> = {
  primary:
    "border-ink bg-ink font-[550] text-on-ink hover:not-disabled:bg-[color-mix(in_srgb,var(--color-ink)_88%,var(--color-bg))]",
  danger:
    "border-danger bg-danger font-[550] text-on-ink hover:not-disabled:bg-[color-mix(in_srgb,var(--color-danger)_88%,var(--color-bg))]",
  secondary:
    "border-transparent bg-control font-medium text-text hover:not-disabled:bg-chip",
  "danger-text":
    "border-transparent bg-control font-medium text-danger hover:not-disabled:bg-chip",
};
const buttonSizes = {
  md: "h-8 gap-1.75 px-3",
  // Beside a page heading.
  lg: "h-9 gap-1.75 px-3.5",
  // The one action of a panel, across its width.
  wide: "h-9 w-full gap-2 px-3",
};
export const button = (
  variant: ButtonVariant,
  size: keyof typeof buttonSizes = "md",
) =>
  cx(
    "inline-flex items-center justify-center rounded-lg border text-[13px] whitespace-nowrap",
    buttonSizes[size],
    buttonVariants[variant],
  );

// A square button holding one icon. lg is the filled kind beside a page
// heading; sm sits inside a field.
export const iconButton = (
  size: "sm" | "md" | "lg" = "md",
  { danger = false } = {},
) =>
  cx(
    "inline-flex shrink-0 items-center justify-center rounded-lg",
    size === "sm" && "size-6 text-muted hover:not-disabled:bg-hover",
    size === "md" && "size-8 text-muted hover:not-disabled:bg-hover",
    size === "lg" &&
      "size-9 bg-control text-subtle hover:not-disabled:bg-chip light:bg-surface light:ring-1 light:ring-line",
    danger ? "hover:not-disabled:text-danger" : "hover:not-disabled:text-text",
  );

export const textButton =
  "py-1 text-[12px] whitespace-nowrap text-muted hover:not-disabled:text-text";

// Segmented control: the options sit in a recess and the chosen one is
// lifted. Inside a panel or a dialog the recess is the well.
export const tabs = (inset = false) =>
  cx(
    "flex items-center gap-0.5 rounded-[10px] p-0.75 max-sm:overflow-x-auto",
    inset ? "bg-well" : "bg-surface",
  );
// The lift the chosen tab and the current navigation link share.
export const lifted =
  "bg-chip text-text light:shadow-[0_1px_2px_#0000000d] light:ring-1 light:ring-line";
export const tab = (selected: boolean, size: "md" | "sm" = "md") =>
  cx(
    "flex items-center gap-1.75 rounded-[7px] text-[12.5px] font-medium whitespace-nowrap",
    size === "md" ? "h-7 px-2.75" : "h-6.5 px-2.25",
    selected ? lifted : "text-muted hover:text-text",
  );

// A message above or inside the content. Callers set the margin.
export const notice = ({ error = false, inset = false } = {}) =>
  cx(
    "rounded-[10px] border border-line px-3.5 py-2.75 text-[13px] leading-[1.6] wrap-anywhere",
    error ? "text-danger" : "text-subtle",
    inset ? "bg-well" : "bg-surface",
  );
// The retry button at the end of a notice.
export const noticeAction = "ml-2 underline";

export const loading = "flex justify-center px-6.25 py-16.25 text-muted";

// Every empty list is the same quiet line, where its first row would be.
// Callers set the margin.
export const empty =
  "flex min-h-8 flex-wrap items-center gap-x-3.5 gap-y-2 text-[13px] leading-[1.6] text-muted";

// Native modal dialogs, centred with the auto margin preflight removes.
// Fields inside use the field colour as their well.
export const dialog = (width: "md" | "sm" | "xs" = "md") =>
  cx(
    "m-auto w-[calc(100%-32px)] rounded-2xl border border-line bg-surface p-6.5 text-text shadow-[0_24px_80px_#000a] [--color-well:var(--color-field)] backdrop:bg-[#000b] backdrop:[backdrop-filter:blur(4px)] max-sm:p-5",
    width === "md" && "max-w-[560px]",
    width === "sm" && "max-w-[480px]",
    width === "xs" && "max-w-[420px]",
  );
export const dialogHeading = "flex items-center justify-between gap-3";
export const dialogTitle =
  "text-[21px] font-medium tracking-[-0.6px] wrap-anywhere";
// The smaller title of a confirmation or a form.
export const confirmTitle =
  "text-[17px] font-[550] tracking-[-0.3px] wrap-anywhere";
export const dialogLead = "mt-3 text-[13px] leading-[1.6] text-subtle";
// On phones the buttons share the row.
export const dialogActions = "flex justify-end gap-2 max-sm:*:flex-1";

// A value in a recess with a copy button at its end.
export const copyField =
  "flex items-center justify-between gap-2.5 rounded-lg border border-line bg-well py-1 pr-1 pl-3";

// The heading row of a provider's group of models or accounts.
export const groupHeading = "flex min-h-8 items-center gap-2.5";
export const groupTitle = "text-[15px] font-semibold";
export const groupCount = "text-[13px] text-muted";

// On/off switch, tinted with the provider colour when on. The knob then takes
// the surface colour, which stands out on any brand, white included.
export const toggle =
  "relative h-4.5 w-7.5 rounded-[9px] bg-chip transition-[background] duration-150 ease-[ease] after:absolute after:top-0.75 after:left-0.75 after:size-3 after:rounded-full after:bg-muted after:transition-[translate,background] after:duration-150 after:ease-[ease] aria-checked:bg-provider aria-checked:after:translate-x-3 aria-checked:after:bg-surface light:not-aria-checked:bg-line";

// Positioned so visually hidden text in a cell scrolls with the table.
export const tableScroll = "relative overflow-x-auto";
