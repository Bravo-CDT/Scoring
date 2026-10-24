import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '../api/client';
import { TOKEN_STORAGE_KEY, useAdminSession } from './useAdminSession';

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

function stubFetch(...responses: Response[]) {
  const fetchMock = vi.fn<typeof fetch>();
  for (const res of responses) fetchMock.mockResolvedValueOnce(res);
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

/** Runs `fn` inside act and returns what it rejected with (undefined if it resolved). */
async function rejectionOf(fn: () => Promise<unknown>): Promise<unknown> {
  let rejection: unknown;
  await act(async () => {
    await fn().catch((e: unknown) => {
      rejection = e;
    });
  });
  return rejection;
}

/** The Authorization header of the fetch mock's nth call. */
function authHeader(fetchMock: ReturnType<typeof stubFetch>, call: number): string | null {
  return new Headers(fetchMock.mock.calls[call][1]?.headers).get('Authorization');
}

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('useAdminSession', () => {
  it('starts logged out with no stored token', () => {
    const { result } = renderHook(() => useAdminSession());
    expect(result.current.token).toBeNull();
  });

  it('picks up a token stored by an earlier visit', () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'abc');
    const { result } = renderHook(() => useAdminSession());
    expect(result.current.token).toBe('abc');
  });

  it('stores the token on a successful login', async () => {
    const fetchMock = stubFetch(jsonResponse(200, { token: 'tok-1' }));
    const { result } = renderHook(() => useAdminSession());

    await act(() => result.current.login('admin', 'hunter2'));

    expect(result.current.token).toBe('tok-1');
    expect(localStorage.getItem(TOKEN_STORAGE_KEY)).toBe('tok-1');
    expect(fetchMock).toHaveBeenCalledWith('/login', expect.objectContaining({ method: 'POST' }));
    expect(JSON.parse(fetchMock.mock.calls[0][1]?.body as string)).toEqual({
      username: 'admin',
      password: 'hunter2',
    });
  });

  it("rejects login with the server's error message on bad credentials", async () => {
    stubFetch(jsonResponse(401, { error: 'invalid credentials' }));
    const { result } = renderHook(() => useAdminSession());

    const err = await rejectionOf(() => result.current.login('admin', 'wrong'));

    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 401, detail: 'invalid credentials' });
    expect(result.current.token).toBeNull();
    expect(localStorage.getItem(TOKEN_STORAGE_KEY)).toBeNull();
  });

  it('attaches the bearer token to authedFetch requests', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'tok-1');
    const fetchMock = stubFetch(jsonResponse(200, {}));
    const { result } = renderHook(() => useAdminSession());

    const res = await act(() => result.current.authedFetch('/engine/start', { method: 'POST' }));

    expect(res.status).toBe(200);
    expect(fetchMock.mock.calls[0][0]).toBe('/engine/start');
    expect(fetchMock.mock.calls[0][1]?.method).toBe('POST');
    expect(authHeader(fetchMock, 0)).toBe('Bearer tok-1');
  });

  it('clears the token and reports logged-out after any 401', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'tok-1');
    stubFetch(jsonResponse(401, { error: 'missing or invalid bearer token' }));
    const { result } = renderHook(() => useAdminSession());

    const err = await rejectionOf(() => result.current.authedFetch('/scoring/pause', { method: 'POST' }));

    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 401 });
    expect(result.current.token).toBeNull();
    expect(result.current.sessionExpired).toBe(true);
    expect(localStorage.getItem(TOKEN_STORAGE_KEY)).toBeNull();
  });

  it('passes non-401 failures through without logging out', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'tok-1');
    stubFetch(jsonResponse(500, { error: 'boom' }));
    const { result } = renderHook(() => useAdminSession());

    const res = await act(() => result.current.authedFetch('/reload', { method: 'POST' }));

    expect(res.status).toBe(500);
    expect(result.current.token).toBe('tok-1');
  });

  it('logs out without a request when the stored token has been removed', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'tok-1');
    const fetchMock = stubFetch();
    const { result } = renderHook(() => useAdminSession());
    localStorage.removeItem(TOKEN_STORAGE_KEY);

    const err = await rejectionOf(() => result.current.authedFetch('/engine/stop', { method: 'POST' }));

    expect(err).toMatchObject({ status: 401 });
    expect(fetchMock).not.toHaveBeenCalled();
    expect(result.current.token).toBeNull();
  });

  it('keeps a newer login when a request sent with an older token comes back 401', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'old');
    let resolveStale!: (res: Response) => void;
    const fetchMock = vi.fn<typeof fetch>().mockReturnValueOnce(new Promise((r) => (resolveStale = r)));
    fetchMock.mockResolvedValueOnce(jsonResponse(200, { token: 'new' }));
    vi.stubGlobal('fetch', fetchMock);
    const { result } = renderHook(() => useAdminSession());

    const stale = result.current.authedFetch('/engine/start', { method: 'POST' });
    await act(() => result.current.login('admin', 'hunter2'));
    resolveStale(jsonResponse(401, { error: 'missing or invalid bearer token' }));
    await act(() => stale.catch(() => {}));

    expect(result.current.token).toBe('new');
    expect(localStorage.getItem(TOKEN_STORAGE_KEY)).toBe('new');
  });

  it('logout clears the token without flagging it as expired', () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'tok-1');
    const { result } = renderHook(() => useAdminSession());

    act(() => result.current.logout());

    expect(result.current.token).toBeNull();
    expect(result.current.sessionExpired).toBe(false);
    expect(localStorage.getItem(TOKEN_STORAGE_KEY)).toBeNull();
  });
});
