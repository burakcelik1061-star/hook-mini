// Where the API key lives in the browser.
//
// By default in sessionStorage: it is gone when the tab closes. Only when
// the user asks to be remembered does it go to localStorage, which outlives
// the tab and is readable by any script on this origin; the console's
// Content-Security-Policy allows no script but its own, which is what makes
// that acceptable as an opt-in. The key is never put in a URL.

const STORAGE_KEY = 'hookmini.apiKey';

/** The exact shape of a Hook Mini API key: "hm_" + 8 hex + "_" + 64 hex. */
export const API_KEY_PATTERN = /^hm_[0-9a-f]{8}_[0-9a-f]{64}$/;

export interface KeyStore {
  get(): string | null;
  set(key: string, remember: boolean): void;
  clear(): void;
}

/** Minimal Storage, so tests can pass fakes. */
export interface StorageLike {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

export function createKeyStore(session: StorageLike, local: StorageLike): KeyStore {
  return {
    get() {
      return session.getItem(STORAGE_KEY) ?? local.getItem(STORAGE_KEY);
    },
    set(key, remember) {
      // Exactly one copy, in the place the user chose.
      session.removeItem(STORAGE_KEY);
      local.removeItem(STORAGE_KEY);
      (remember ? local : session).setItem(STORAGE_KEY, key);
    },
    clear() {
      session.removeItem(STORAGE_KEY);
      local.removeItem(STORAGE_KEY);
    },
  };
}

/** The prefix shown in the header: enough to tell keys apart, not to use one. */
export function keyHint(key: string | null): string {
  if (!key) return '';
  return key.slice(0, 11) + '…';
}
