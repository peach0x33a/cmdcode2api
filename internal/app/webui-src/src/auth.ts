const STORAGE_KEY = "cmdcode2api_admin_key";

// getStoredKey/setStoredKey/clearStoredKey wrap localStorage with try/catch
// since it can throw (private browsing, disabled site data) — callers treat
// a thrown read/write the same as "no key stored" rather than crashing the
// app over a login convenience.
export function getStoredKey(): string {
  try {
    return localStorage.getItem(STORAGE_KEY) || "";
  } catch {
    return "";
  }
}

export function setStoredKey(key: string): void {
  try {
    localStorage.setItem(STORAGE_KEY, key);
  } catch {
    // ignore — the key will just be asked for again next request
  }
}

export function clearStoredKey(): void {
  try {
    localStorage.removeItem(STORAGE_KEY);
  } catch {
    // ignore
  }
}
