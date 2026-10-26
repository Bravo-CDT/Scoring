import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  ApiError,
  type AuthedFetch,
  type CheckResult,
  getResults,
  getStatus,
  reload,
  restartService,
  setCountdown,
  startEngine,
  type Status,
  stopService,
} from './client';

function stubFetch(response: Partial<Response>) {
  const fetchMock = vi.fn().mockResolvedValue(response);
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('getStatus', () => {
  it('fetches /status and returns the parsed body', async () => {
    const body: Status = {
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
        {
          name: 'web',
          up: true,
          routine_enabled: true,
          checking: false,
          scoring_paused: false,
          latency_ms: 4,
          consecutive_failures: 0,
          host: '10.0.0.1',
          check_type: 'tcp_connect',
        },
      ],
      networks: [],
      hosts: [],
      credentials_enabled: false,
      injects_enabled: false,
    };
    const fetchMock = stubFetch({ ok: true, status: 200, json: async () => body });

    const status = await getStatus();

    expect(fetchMock).toHaveBeenCalledWith('/status', expect.anything());
    expect(status.blue_score).toBe(980);
    expect(status.round).toBe(5);
    expect(status.services[0].name).toBe('web');
  });

  it('throws on a non-OK response', async () => {
    stubFetch({ ok: false, status: 500, json: async () => ({ error: 'internal server error' }) });

    const err = await getStatus().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 500, message: 'GET /status: 500 internal server error' });
  });
});

describe('getResults', () => {
  it('fetches the service\'s results with the limit and returns the parsed body', async () => {
    const body: CheckResult[] = [
      { id: 2, round: 4, timestamp: '2026-10-18T12:00:05Z', success: false, latency_ms: 0, detail: 'refused' },
      { id: 1, round: 3, timestamp: '2026-10-18T12:00:00Z', success: true, latency_ms: 4 },
    ];
    const fetchMock = stubFetch({ ok: true, status: 200, json: async () => body });

    const results = await getResults('web', 20);

    expect(fetchMock).toHaveBeenCalledWith('/services/web/results?limit=20', expect.anything());
    expect(results).toEqual(body);
  });

  it('leaves the limit to the API when none is given', async () => {
    const fetchMock = stubFetch({ ok: true, status: 200, json: async () => [] });

    await getResults('web');

    expect(fetchMock).toHaveBeenCalledWith('/services/web/results', expect.anything());
  });

  it('escapes the service name into a single path segment', async () => {
    const fetchMock = stubFetch({ ok: true, status: 200, json: async () => [] });

    await getResults('a/b ?c');

    expect(fetchMock).toHaveBeenCalledWith('/services/a%2Fb%20%3Fc/results', expect.anything());
  });

  it('throws on a non-OK response', async () => {
    stubFetch({ ok: false, status: 404, json: async () => ({ error: 'unknown service' }) });

    const err = await getResults('nope').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 404, message: 'GET /services/nope/results: 404 unknown service' });
  });
});

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

/** An AuthedFetch that answers every request with `res`, recording the calls. */
function mockSend(res: Response) {
  return vi.fn<AuthedFetch>().mockResolvedValue(res);
}

/** The JSON body of the mock's only request, or undefined if it had none. */
function sentBody(send: ReturnType<typeof mockSend>): unknown {
  const body = send.mock.calls[0][1]?.body;
  return body === undefined ? undefined : JSON.parse(body as string);
}

describe('admin actions', () => {
  it('POSTs through the given AuthedFetch and returns the status with its failed list', async () => {
    const send = mockSend(jsonResponse(200, { round: 3, failed: [{ service: 'dns', error: 'bad port' }] }));

    const res = await startEngine(send);

    expect(send).toHaveBeenCalledWith('/engine/start', expect.objectContaining({ method: 'POST' }));
    expect(sentBody(send)).toBeUndefined();
    expect(res.failed).toEqual([{ service: 'dns', error: 'bad port' }]);
  });

  it("escapes the service name and throws the API's error on failure", async () => {
    const send = mockSend(jsonResponse(404, { error: 'engine: unknown service "a/b"' }));

    const err = await stopService(send, 'a/b').catch((e: unknown) => e);

    expect(send).toHaveBeenCalledWith('/services/a%2Fb/stop', expect.anything());
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 404, detail: 'engine: unknown service "a/b"' });
  });

  it('sends pause_scoring on restart only when asked', async () => {
    const plain = mockSend(jsonResponse(200, { failed: [] }));
    await restartService(plain, 'web');
    expect(sentBody(plain)).toBeUndefined();

    const paused = mockSend(jsonResponse(200, { failed: [] }));
    await restartService(paused, 'web', { pauseScoring: true });
    expect(paused).toHaveBeenCalledWith('/services/web/restart', expect.anything());
    expect(sentBody(paused)).toEqual({ pause_scoring: true });
  });

  it('sends the countdown target, and an explicit null to clear it', async () => {
    const set = mockSend(jsonResponse(200, {}));
    await setCountdown(set, '2026-10-18T14:00:00.000Z');
    expect(set).toHaveBeenCalledWith('/countdown', expect.anything());
    expect(sentBody(set)).toEqual({ target: '2026-10-18T14:00:00.000Z' });

    const clear = mockSend(jsonResponse(200, {}));
    await setCountdown(clear, null);
    expect(clear.mock.calls[0][1]?.body).toBe('{"target":null}');
  });
});

describe('reload', () => {
  it('returns the changes and failures of a successful reload', async () => {
    const body = { added: ['ftp'], removed: [], failed: [{ service: 'ftp', error: 'bad port' }] };
    const send = mockSend(jsonResponse(200, body));

    expect(await reload(send)).toEqual(body);
  });

  it('resolves a failed reload with its error and the changes made before it', async () => {
    const body = { added: ['ftp'], removed: [], failed: [], error: 'config: line 3: bad yaml' };

    expect(await reload(mockSend(jsonResponse(500, body)))).toEqual(body);
  });

  it('throws on any other failure', async () => {
    const err = await reload(mockSend(jsonResponse(403, { error: 'nope' }))).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 403, detail: 'nope' });
  });
});
