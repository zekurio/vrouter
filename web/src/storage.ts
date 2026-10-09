// Browser storage for UI preferences. Storage can be unavailable, and then a
// choice lasts for this page load only. Tokens and key secrets never go here.

export function readStored(key: string) {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

export function writeStored(key: string, value: string | null) {
  try {
    if (value === null) localStorage.removeItem(key);
    else localStorage.setItem(key, value);
  } catch {
    // Storage is unavailable.
  }
}
