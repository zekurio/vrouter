import type { CSSProperties } from "react";

// Providers are identified by the lower-case ids the gateway stores.
const labels: Record<string, string> = {
  codex: "Codex",
  claude: "Claude",
  grok: "Grok",
  gemini: "Gemini",
  other: "Other",
};
export const providerLabel = (provider: string) => labels[provider] || provider;
export const providerName = (provider: string) =>
  provider === "codex" ? "OpenAI" : providerLabel(provider);
const brands: Record<string, string> = {
  codex: "openai",
  claude: "claude",
  grok: "xai",
  gemini: "gemini",
};
export const providerColor = (provider: string) =>
  `var(--brand-${brands[provider] ?? "other"})`;
export function ProviderBrand({
  provider,
  small = false,
}: {
  provider: string;
  small?: boolean;
}) {
  const asset = brands[provider];
  if (!asset || asset === "gemini") return null;
  return (
    <span
      className={`provider-mark ${small ? "small" : ""}`}
      style={
        {
          maskImage: `url(/brands/${asset}.svg)`,
          WebkitMaskImage: `url(/brands/${asset}.svg)`,
        } as CSSProperties
      }
      aria-hidden="true"
    />
  );
}
