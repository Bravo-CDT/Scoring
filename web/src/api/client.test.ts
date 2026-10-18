import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError, type CheckResult, getResults, getStatus, type Status } from './client';

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
          scoring_paused: false,
          latency_ms: 4,
          consecutive_failures: 0,
        },
      ],
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
