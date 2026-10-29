import type { CSSProperties } from "react";
import { cx } from "./ui";

declare module "react" {
  interface CSSProperties {
    // Brand colour behind the provider utilities, such as bg-provider.
    "--color-provider"?: string;
  }
}

// Providers are identified by the lower-case ids vrouter stores.
const labels: Record<string, string> = {
  codex: "Codex",
  claude: "Claude",
  grok: "Grok",
  gemini: "Gemini",
  other: "Other",
};
export const providerLabel = (provider: string) => labels[provider] ?? provider;
export const providerName = (provider: string) =>
  provider === "codex" ? "OpenAI" : providerLabel(provider);
const brands: Record<string, string> = {
  codex: "openai",
  claude: "claude",
  grok: "xai",
  gemini: "gemini",
};
export const providerColor = (provider: string) =>
  `var(--color-brand-${brands[provider] ?? "other"})`;
export const providerStyle = (provider: string): CSSProperties => ({
  "--color-provider": providerColor(provider),
});
export function ProviderBrand({
  provider,
  small = false,
}: {
  provider: string;
  small?: boolean;
}) {
  const asset = brands[provider];
  if (asset === undefined || asset === "gemini") return null;
  return (
    <span
      className={cx(
        "inline-block shrink-0 bg-provider mask-contain mask-center mask-no-repeat",
        small ? "size-4.75" : "size-5.5",
      )}
      style={{
        maskImage: `url(/brands/${asset}.svg)`,
        WebkitMaskImage: `url(/brands/${asset}.svg)`,
      }}
      aria-hidden="true"
    />
  );
}
