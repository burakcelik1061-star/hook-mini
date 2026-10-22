// The signed-in session: the key, a client that uses it, and the tenant's
// request budget as the API last reported it.

import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from 'react';
import { createClient, type Client, type RateLimit } from './api';
import { createKeyStore, storageOrMemory } from './auth';
import { navigate, paths } from './router';

const store = createKeyStore(
  storageOrMemory(() => window.sessionStorage),
  storageOrMemory(() => window.localStorage),
);

interface Session {
  apiKey: string | null;
  client: Client;
  rateLimit: RateLimit | null;
  signIn(key: string, remember: boolean): void;
  signOut(): void;
}

const Ctx = createContext<Session | null>(null);

export function SessionProvider({ children }: { children: ReactNode }) {
  const [apiKey, setApiKey] = useState<string | null>(() => store.get());
  const [rateLimit, setRateLimit] = useState<RateLimit | null>(null);

  const signOut = useCallback(() => {
    store.clear();
    setApiKey(null);
    setRateLimit(null);
    navigate(paths.signIn);
  }, []);

  const client = useMemo(
    () =>
      createClient({
        getKey: () => store.get(),
        onRateLimit: setRateLimit,
        // The key was revoked (or the tenant deleted): back to sign-in.
        onUnauthorized: signOut,
      }),
    [signOut],
  );

  const signIn = useCallback((key: string, remember: boolean) => {
    store.set(key, remember);
    setApiKey(key);
  }, []);

  const value = useMemo(() => ({ apiKey, client, rateLimit, signIn, signOut }), [apiKey, client, rateLimit, signIn, signOut]);
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useSession(): Session {
  const s = useContext(Ctx);
  if (!s) throw new Error('useSession outside SessionProvider');
  return s;
}

/** A client that only exists while signed in: a throwaway one for sign-in checks. */
export function clientForKey(key: string): Client {
  return createClient({ getKey: () => key });
}
