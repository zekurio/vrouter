import type { Role } from "./api";
import type { Scope } from "./ConnectDialog";
import { cx } from "./ui";

const tones = {
  neutral: "bg-chip text-subtle light:bg-control",
  accent: "bg-accent/14 text-accent",
  green: "bg-green/13 text-green",
  amber: "bg-amber/14 text-amber",
};

// Role, scope and "You" labels all use this one pill. Place it in a
// badgeRow to line it up with the text beside it.
export function Badge({
  tone = "neutral",
  children,
}: {
  tone?: keyof typeof tones;
  children: string;
}) {
  return (
    <span
      className={cx(
        "inline-flex h-4.5 flex-none items-center rounded-full px-1.75 text-[11px] leading-none font-semibold whitespace-nowrap",
        tones[tone],
      )}
    >
      {children}
    </span>
  );
}

// Text and the badges beside it, centred on one line.
export const badgeRow = "flex flex-wrap items-center gap-x-2 gap-y-1";

export function RoleBadge({ role }: { role: Role }) {
  return role === "admin" ? (
    <Badge tone="accent">Admin</Badge>
  ) : (
    <Badge>Member</Badge>
  );
}

export function ScopeBadge({ scope }: { scope: Scope }) {
  return scope === "global" ? (
    <Badge tone="green">Shared</Badge>
  ) : (
    <Badge tone="amber">Personal</Badge>
  );
}
