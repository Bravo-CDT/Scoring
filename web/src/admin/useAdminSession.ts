import { useCallback, useEffect, useState } from 'react';
import { ApiError, type AuthedFetch, blueLogin, fetchWithToken, login as apiLogin, responseError } from '../api/client';

/** The localStorage key the admin's bearer token is kept under. */
export const TOKEN_STORAGE_KEY = 'admin_token';

/** The localStorage key the blue team's bearer token is kept under. */
export const BLUE_TOKEN_STORAGE_KEY = 'blue_token';

// Storage access throws where the browser blocks it (some private modes, or
// site data disabled); the admin then simply stays logged out.

function readToken(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function writeToken(key: string, token: string | null) {
  try {
    if (token === null) {
      localStorage.removeItem(key);
    } else {
      localStorage.setItem(key, token);
    }
  } catch {
    // See above.
  }
}

export interface Session {
  /** The bearer token; null while logged out. */
  token: string | null;
  /**
   * Whether the session ended because the API rejected its token (or the
   * token vanished from storage), rather than by logging out: the login
   * screen says so.
   */
  sessionExpired: boolean;
  /** Logs in, storing the token; rejects with the API's error on bad credentials. */
  login: (username: string, password: string) => Promise<void>;
  logout: () => void;
  /**
   * Sends an admin request with the token attached. A 401 ends the session
   * before rejecting, so every admin action that goes through here returns
   * the admin to the login screen when the token stops working.
   */
  authedFetch: AuthedFetch;
}

/** The admin's login session; see useSession. */
export function useAdminSession(): Session {
  return useSession(TOKEN_STORAGE_KEY, apiLogin);
}

/** The blue team's login session, for the credentials page; see useSession. */
export function useBlueSession(): Session {
  return useSession(BLUE_TOKEN_STORAGE_KEY, blueLogin);
}

/**
 * A login session whose token is kept under storageKey and obtained with
 * loginFn (which must be stable: a module-level function). localStorage is
 * the source of truth for the token: authedFetch reads it afresh for every
 * request, so a token cleared
 * by hand, or by logging out in another tab, ends the session here at the
 * next action instead of being used from memory.
 *
 * Call it once, at the top of the view, and pass what it returns down.
 * Each call holds its own state, so a second call elsewhere would not see
 * this one's login.
 */
function useSession(storageKey: string, loginFn: (username: string, password: string) => Promise<string>): Session {
  const [token, setToken] = useState(() => readToken(storageKey));
  const [sessionExpired, setSessionExpired] = useState(false);

  // Follow logins and logouts in other tabs.
  useEffect(() => {
    const onStorage = (event: StorageEvent) => {
      if (event.key === storageKey || event.key === null) {
        setToken(readToken(storageKey));
      }
    };
    window.addEventListener('storage', onStorage);
    return () => window.removeEventListener('storage', onStorage);
  }, [storageKey]);

  const endSession = useCallback(
    (expired: boolean) => {
      writeToken(storageKey, null);
      setToken(null);
      setSessionExpired(expired);
    },
    [storageKey],
  );

  const logout = useCallback(() => endSession(false), [endSession]);

  const login = useCallback(
    async (username: string, password: string) => {
      const newToken = await loginFn(username, password);
      writeToken(storageKey, newToken);
      setToken(newToken);
      setSessionExpired(false);
    },
    [storageKey, loginFn],
  );

  const authedFetch = useCallback<AuthedFetch>(
    async (path, init) => {
      const method = init?.method ?? 'GET';
      const sent = readToken(storageKey);
      if (sent === null) {
        endSession(true);
        throw new ApiError(method, path, 401, 'not logged in');
      }
      const res = await fetchWithToken(sent, path, init);
      if (res.status === 401) {
        // A request sent before a fresh login can come back 401 after it;
        // that must not throw the new session away.
        const current = readToken(storageKey);
        if (current === null || current === sent) {
          endSession(true);
        }
        throw await responseError(method, path, res);
      }
      return res;
    },
    [endSession, storageKey],
  );

  return { token, sessionExpired, login, logout, authedFetch };
}
