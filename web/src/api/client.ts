// Typed wrappers for the scoring engine's JSON API. Every request the
// frontend makes goes through this module.
//
// Field names deliberately match the Go structs' JSON tags (snake_case), so
// these types read as a direct transcription of the wire format and cannot
// silently drift from it. Timestamps are RFC 3339 strings, as Go encodes
// time.Time.

/** One service's row in GET /status (engine.ServiceStatus). */
export interface ServiceStatus {
  name: string;
  /** Whether the latest check succeeded; false before any check. */
  up: boolean;
  /** Whether the service's check routine is running. */
  routine_enabled: boolean;
  /** The service's own scoring pause; the global one is Status.scoring_paused. */
  scoring_paused: boolean;
  /** When the latest check ran; absent before any check. */
  last_check_time?: string;
  latency_ms: number;
  consecutive_failures: number;
}

/** The response of GET /status (engine.Status). */
export interface Status {
  session_name: string;
  starting_score: number;
  blue_score: number;
  red_score: number;
  engine_state: 'running' | 'stopped';
  scoring_paused: boolean;
  server_time: string;
  /** The number of the last round recorded. */
  round: number;
  /** When the next round is due; Go's zero time until a round is recorded. */
  next_round_at: string;
  /** The admin-set time the countdown runs to; absent while unset. */
  countdown_target?: string;
  services: ServiceStatus[];
}

/** A non-2xx response from the API. */
export class ApiError extends Error {
  readonly status: number;

  constructor(method: string, path: string, status: number, detail?: string) {
    super(`${method} ${path}: ${status}${detail ? ` ${detail}` : ''}`);
    this.name = 'ApiError';
    this.status = status;
  }
}

/**
 * Reads the API's `{"error": "..."}` body, if there is one. Every API
 * response is JSON, but a proxy or a dev server in front of it may not be.
 */
async function errorDetail(res: Response): Promise<string | undefined> {
  try {
    const body: unknown = await res.json();
    if (body && typeof body === 'object' && 'error' in body && typeof body.error === 'string') {
      return body.error;
    }
  } catch {
    // Not JSON; the status code alone will have to do.
  }
  return undefined;
}

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(path, { headers: { Accept: 'application/json' } });
  if (!res.ok) {
    throw new ApiError('GET', path, res.status, await errorDetail(res));
  }
  return (await res.json()) as T;
}

/** GET /status: the engine's point-in-time view of the session. */
export function getStatus(): Promise<Status> {
  return getJSON<Status>('/status');
}

/** One check attempt in GET /services/{name}/results (api.checkResultResponse). */
export interface CheckResult {
  id: number;
  round: number;
  timestamp: string;
  success: boolean;
  latency_ms: number;
  /**
   * The check's explanation of its outcome; absent when empty. It is text
   * from the checked service itself, untrusted: render it only as text.
   */
  detail?: string;
}

/**
 * GET /services/{name}/results: the service's latest check results, newest
 * first. `limit` is how many to return; the API defaults it (to 50) when
 * omitted and caps it (at 500).
 */
export function getResults(name: string, limit?: number): Promise<CheckResult[]> {
  const query = limit === undefined ? '' : `?${new URLSearchParams({ limit: String(limit) })}`;
  return getJSON<CheckResult[]>(`/services/${encodeURIComponent(name)}/results${query}`);
}
