import { useEffect, useState } from "react";
import { Monitor, Moon, Sun } from "lucide-react";
import { readStored, writeStored } from "./storage";

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

const themeKey = "vrouter-theme";
const storedTheme = (): Theme => {
  const value = readStored(themeKey);
  return value === "system" || value === "light" ? value : "dark";
};

// The chosen theme, following the system setting when it is "system".
export function useTheme() {
  const [theme, setTheme] = useState<Theme>(storedTheme);
  const [systemLight, setSystemLight] = useState(
    () => matchMedia("(prefers-color-scheme: light)").matches,
  );
  const light = theme === "system" ? systemLight : theme === "light";
  useEffect(() => {
    const system = matchMedia("(prefers-color-scheme: light)");
    const scheme = () => setSystemLight(system.matches);
    system.addEventListener("change", scheme);
    return () => system.removeEventListener("change", scheme);
  }, []);
  useEffect(() => {
    document.documentElement.dataset["theme"] = light ? "light" : "dark";
    writeStored(themeKey, theme);
  }, [light, theme]);
  return { theme, setTheme };
}
