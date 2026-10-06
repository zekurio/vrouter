import type { CSSProperties } from "react";

export const providerName = (provider: string) =>
  provider === "Codex" ? "OpenAI" : provider;
const colors: Record<string, string> = {
  Codex: "#79bce9",
  OpenAI: "#79bce9",
  Claude: "#e6a27b",
  Grok: "#87c99b",
  Gemini: "#b6a3eb",
};
export const providerColor = (provider: string) =>
  colors[provider] || "#b0b4bd";
export function ProviderBrand({
  provider,
  small = false,
}: {
  provider: string;
  small?: boolean;
}) {
  const asset =
    provider === "Codex" || provider === "OpenAI"
      ? "openai"
      : provider === "Claude"
        ? "claude"
        : provider === "Grok"
          ? "xai"
          : null;
  if (!asset) return null;
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
