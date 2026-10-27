import { Monitor, Moon, Sun } from "lucide-react";

export const themes = ["system", "dark", "light"] as const;
export type Theme = (typeof themes)[number];
export const themeIcons = { system: Monitor, dark: Moon, light: Sun };
export const themeLabels: Record<Theme, string> = {
  system: "System",
  dark: "Dark",
  light: "Light",
};
export const nextThemes: Record<Theme, Theme> = {
  system: "dark",
  dark: "light",
  light: "system",
};
