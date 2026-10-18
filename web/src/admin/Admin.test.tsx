import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Status } from '../api/client';
import { render } from '../test/render';
import { Admin } from './Admin';
import { TOKEN_STORAGE_KEY } from './useAdminSession';

const status: Status = {
  session_name: 'demo',
  starting_score: 1000,
  blue_score: 980,
  red_score: 20,
  engine_state: 'running',
  scoring_paused: false,
  server_time: '2026-10-18T12:00:00Z',
  round: 5,
  next_round_at: '2026-10-18T12:00:05Z',
  services: [
    { name: 'web', up: true, routine_enabled: true, scoring_paused: false, latency_ms: 4, consecutive_failures: 0 },
  ],
};

function jsonResponse(httpStatus: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status: httpStatus, headers: { 'Content-Type': 'application/json' } });
}

type Route = (init: RequestInit | undefined) => Response;

/**
 * Stubs fetch with a route table keyed by "METHOD path". GET /status always
 * answers with `status`; an unrouted request fails the test.
 */
function stubApi(routes: Record<string, Route> = {}) {
  const fetchMock = vi.fn<typeof fetch>(async (input, init) => {
    const key = `${init?.method ?? 'GET'} ${String(input)}`;
    if (key === 'GET /status') return jsonResponse(200, status);
    const route = routes[key];
    if (!route) throw new Error(`unexpected request: ${key}`);
    return route(init);
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

/** The "METHOD path" of every request but the status polls. */
function adminRequests(fetchMock: ReturnType<typeof stubApi>): string[] {
  return fetchMock.mock.calls
    .map(([input, init]) => `${init?.method ?? 'GET'} ${String(input)}`)
    .filter((key) => key !== 'GET /status');
}

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('Admin', () => {
  it("shows the server's error on a wrong password, then the console on the right one", async () => {
    stubApi({
      'POST /login': (init) => {
        const { password } = JSON.parse(init?.body as string) as { password: string };
        return password === 'right'
          ? jsonResponse(200, { token: 'tok' })
          : jsonResponse(401, { error: 'invalid credentials' });
      },
    });
    const user = userEvent.setup();
    render(<Admin />);

    await user.type(screen.getByLabelText(/Username/), 'admin');
    await user.type(screen.getByLabelText(/Password/), 'wrong');
    await user.click(screen.getByRole('button', { name: 'Log in' }));
    expect(await screen.findByText('invalid credentials')).toBeInTheDocument();

    await user.clear(screen.getByLabelText(/Password/));
    await user.type(screen.getByLabelText(/Password/), 'right');
    await user.click(screen.getByRole('button', { name: 'Log in' }));
    expect(await screen.findByRole('button', { name: 'Stop engine' })).toBeInTheDocument();
    expect(localStorage.getItem(TOKEN_STORAGE_KEY)).toBe('tok');
  });

  it('asks before stopping the engine, and sends nothing when cancelled', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'tok');
    const fetchMock = stubApi({
      'POST /engine/stop': () => jsonResponse(200, { ...status, engine_state: 'stopped', server_time: '2026-10-18T12:00:01Z' }),
    });
    const user = userEvent.setup();
    render(<Admin />);

    await user.click(await screen.findByRole('button', { name: 'Stop engine' }));
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent('Stop the engine?');
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(adminRequests(fetchMock)).toEqual([]);

    await user.click(screen.getByRole('button', { name: 'Stop engine' }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Stop engine' }));

    expect(await screen.findByText('Stop engine: done')).toBeInTheDocument();
    expect(adminRequests(fetchMock)).toEqual(['POST /engine/stop']);
    expect(new Headers(fetchMock.mock.calls.at(-1)?.[1]?.headers).get('Authorization')).toBe('Bearer tok');
    // The action's answer shows at once: the engine is stopped.
    expect(screen.getByRole('button', { name: 'Start engine' })).toBeInTheDocument();
  });

  it('asks before stopping a service', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'tok');
    const fetchMock = stubApi({ 'POST /services/web/stop': () => jsonResponse(200, status) });
    const user = userEvent.setup();
    render(<Admin />);

    await user.click(await screen.findByRole('button', { name: 'Stop web' }));
    expect(adminRequests(fetchMock)).toEqual([]);
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Stop web' }));

    expect(await screen.findByText('Stop web: done')).toBeInTheDocument();
    expect(adminRequests(fetchMock)).toEqual(['POST /services/web/stop']);
  });

  it('pauses scoring at once, without asking', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'tok');
    const fetchMock = stubApi({ 'POST /scoring/pause': () => jsonResponse(200, status) });
    const user = userEvent.setup();
    render(<Admin />);

    await user.click(await screen.findByRole('button', { name: 'Pause scoring' }));

    expect(await screen.findByText('Pause scoring: done')).toBeInTheDocument();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(adminRequests(fetchMock)).toEqual(['POST /scoring/pause']);
  });

  it('returns to the login screen when an action gets a 401', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'revoked');
    stubApi({
      'POST /services/web/restart': () => jsonResponse(401, { error: 'missing or invalid bearer token' }),
    });
    const user = userEvent.setup();
    render(<Admin />);

    await user.click(await screen.findByRole('button', { name: 'Restart web' }));

    expect(await screen.findByRole('button', { name: 'Log in' })).toBeInTheDocument();
    expect(screen.getByText(/session has expired/)).toBeInTheDocument();
    expect(localStorage.getItem(TOKEN_STORAGE_KEY)).toBeNull();
  });

  it('lists what a reload changed and the routines that failed to start', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'tok');
    stubApi({
      'POST /reload': () =>
        jsonResponse(200, { added: ['ftp'], removed: ['dns'], failed: [{ service: 'ftp', error: 'bad port' }] }),
    });
    const user = userEvent.setup();
    render(<Admin />);

    await user.click(await screen.findByRole('button', { name: 'Reload config' }));

    expect(await screen.findByText('Reloaded, but some routines failed to start')).toBeInTheDocument();
    expect(screen.getByText('Added: ftp')).toBeInTheDocument();
    expect(screen.getByText('Removed: dns')).toBeInTheDocument();
    expect(screen.getByText('Failed to start ftp: bad port')).toBeInTheDocument();
  });

  it('sets the countdown as an absolute instant, and clears it with null', async () => {
    localStorage.setItem(TOKEN_STORAGE_KEY, 'tok');
    const bodies: unknown[] = [];
    stubApi({
      'POST /countdown': (init) => {
        const body = JSON.parse(init?.body as string) as { target: string | null };
        bodies.push(body);
        return jsonResponse(200, {
          ...status,
          server_time: `2026-10-18T12:00:0${bodies.length}Z`,
          countdown_target: body.target ?? undefined,
        });
      },
    });
    const user = userEvent.setup();
    render(<Admin />);

    await user.type(await screen.findByLabelText(/Count down to/), '2026-10-18T14:30');
    await user.click(screen.getByRole('button', { name: 'Set countdown' }));
    expect(await screen.findByText('Set countdown: done')).toBeInTheDocument();
    expect(bodies[0]).toEqual({ target: new Date('2026-10-18T14:30').toISOString() });

    await user.click(screen.getByRole('button', { name: 'Clear countdown' }));
    expect(await screen.findByText('Clear countdown: done')).toBeInTheDocument();
    expect(bodies[1]).toEqual({ target: null });
  });
});
