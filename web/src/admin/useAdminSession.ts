import { useCallback, useEffect, useState } from 'react';
import { ApiError, type AuthedFetch, fetchWithToken, login as apiLogin, responseError } from '../api/client';

/** The localStorage key the admin's bearer token is kept under. */
export const TOKEN_STORAGE_KEY = 'admin_token';

// Storage access throws where the browser blocks it (some private modes, or
// site data disabled); the admin then simply stays logged out.

function readToken(): string | null {
  try {
    return localStorage.getItem(TOKEN_STORAGE_KEY);
  } catch {
    return null;
  }
}

function writeToken(token: string | null) {
  try {
    if (token === null) {
      localStorage.removeItem(TOKEN_STORAGE_KEY);
    } else {
      localStorage.setItem(TOKEN_STORAGE_KEY, token);
    }
  } catch {
    // See above.
  }
}

export interface AdminSession {
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

/**
 * The admin's login session. localStorage is the source of truth for the
 * token: authedFetch reads it afresh for every request, so a token cleared
 * by hand, or by logging out in another tab, ends the session here at the
 * next action instead of being used from memory.
 *
 * Call it once, at the top of the admin view, and pass what it returns down.
 * Each call holds its own state, so a second call elsewhere would not see
 * this one's login.
 */
export function useAdminSession(): AdminSession {
  const [token, setToken] = useState(readToken);
  const [sessionExpired, setSessionExpired] = useState(false);

  // Follow logins and logouts in other tabs.
  useEffect(() => {
    const onStorage = (event: StorageEvent) => {
      if (event.key === TOKEN_STORAGE_KEY || event.key === null) {
        setToken(readToken());
      }
    };
    window.addEventListener('storage', onStorage);
    return () => window.removeEventListener('storage', onStorage);
  }, []);

  const endSession = useCallback((expired: boolean) => {
    writeToken(null);
    setToken(null);
    setSessionExpired(expired);
  }, []);

  const logout = useCallback(() => endSession(false), [endSession]);

  const login = useCallback(async (username: string, password: string) => {
    const newToken = await apiLogin(username, password);
    writeToken(newToken);
    setToken(newToken);
    setSessionExpired(false);
  }, []);

  const authedFetch = useCallback<AuthedFetch>(
    async (path, init) => {
      const method = init?.method ?? 'GET';
      const sent = readToken();
      if (sent === null) {
        endSession(true);
        throw new ApiError(method, path, 401, 'not logged in');
      }
      const res = await fetchWithToken(sent, path, init);
      if (res.status === 401) {
        // A request sent before a fresh login can come back 401 after it;
        // that must not throw the new session away.
        const current = readToken();
        if (current === null || current === sent) {
          endSession(true);
        }
        throw await responseError(method, path, res);
      }
      return res;
    },
    [endSession],
  );

  return { token, sessionExpired, login, logout, authedFetch };
}
