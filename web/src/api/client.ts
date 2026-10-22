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
  /** Whether the service's check is part of the round in progress right now. */
  checking: boolean;
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
  /** The API's own `error` message, when the response carried one. */
  readonly detail?: string;

  constructor(method: string, path: string, status: number, detail?: string) {
    super(`${method} ${path}: ${status}${detail ? ` ${detail}` : ''}`);
    this.name = 'ApiError';
    this.status = status;
    this.detail = detail;
  }
}

/**
 * A message for showing `err` to a person: the API's own message when it
 * gave one, else the error's.
 */
export function describeError(err: unknown): string {
  if (err instanceof ApiError && err.detail) return err.detail;
  return err instanceof Error ? err.message : String(err);
}

/**
 * Reads the API's `{"error": "..."}` body, if there is one. Every API
 * response is JSON, but a proxy or a dev server in front of it may not be.
 */
async function errorDetail(res: Response): Promise<string | undefined> {
  return errorMessageOf(await jsonOrUndefined(res));
}

/** The ApiError for a non-2xx `res` to `method path`, with the API's message. */
export async function responseError(method: string, path: string, res: Response): Promise<ApiError> {
  return new ApiError(method, path, res.status, await errorDetail(res));
}

/** The response's JSON body, or undefined if it is not JSON. */
async function jsonOrUndefined(res: Response): Promise<unknown> {
  try {
    return await res.json();
  } catch {
    return undefined;
  }
}

/** The `error` message of an API error body, if it has one. */
function errorMessageOf(body: unknown): string | undefined {
  if (body && typeof body === 'object' && 'error' in body && typeof body.error === 'string') {
    return body.error;
  }
  return undefined;
}

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(path, { headers: { Accept: 'application/json' } });
  if (!res.ok) {
    throw await responseError('GET', path, res);
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

// Admin endpoints.
//
// Every admin call takes an AuthedFetch rather than a token: the caller (the
// admin session hook) owns where the token lives and what happens when the
// API rejects it, and this module stays free of storage and React concerns.

/**
 * Sends an admin request with the session's bearer token attached. It
 * resolves with the response for any status but 401, which it rejects with
 * an ApiError after ending the session.
 */
export type AuthedFetch = (path: string, init?: RequestInit) => Promise<Response>;

/**
 * Sends `init` to `path` with `token` as its bearer token. The admin session
 * hook builds its AuthedFetch on this; nothing else should need it.
 */
export function fetchWithToken(token: string, path: string, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers);
  headers.set('Authorization', `Bearer ${token}`);
  return fetch(path, { ...init, headers });
}

/**
 * POST /login: exchanges the admin credentials for a bearer token. Bad
 * credentials reject with an ApiError whose `detail` is the API's message.
 */
export async function login(username: string, password: string): Promise<string> {
  const res = await fetch('/login', {
    method: 'POST',
    headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  });
  if (!res.ok) {
    throw await responseError('POST', '/login', res);
  }
  const body = (await res.json()) as { token: string };
  return body.token;
}

/** A routine that failed to start (engine.StartFailure). */
export interface StartFailure {
  service: string;
  error: string;
}

/**
 * The answer of the admin routes that start routines: the current status
 * plus the routines that failed to start, an always-present list that is
 * empty when everything started.
 */
export interface StartResponse extends Status {
  failed: StartFailure[];
}

/** The response of POST /reload (api.reloadResponse). */
export interface ReloadResponse {
  added: string[];
  removed: string[];
  failed: StartFailure[];
  /**
   * Why the reload failed as a whole (an invalid file, a store error); absent
   * when it succeeded. `added` and `removed` then list what it changed before
   * failing.
   */
  error?: string;
}

function postInit(body?: unknown): RequestInit {
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (body === undefined) {
    return { method: 'POST', headers };
  }
  headers['Content-Type'] = 'application/json';
  return { method: 'POST', headers, body: JSON.stringify(body) };
}

async function postJSON<T>(send: AuthedFetch, path: string, body?: unknown): Promise<T> {
  const res = await send(path, postInit(body));
  if (!res.ok) {
    throw await responseError('POST', path, res);
  }
  return (await res.json()) as T;
}

function servicePath(name: string, action: string): string {
  return `/services/${encodeURIComponent(name)}/${action}`;
}

/** POST /engine/start; routines that fail to start are listed, not thrown. */
export function startEngine(send: AuthedFetch): Promise<StartResponse> {
  return postJSON(send, '/engine/start');
}

/** POST /engine/stop: stops every check routine and scoring with them. */
export function stopEngine(send: AuthedFetch): Promise<Status> {
  return postJSON(send, '/engine/stop');
}

/** POST /scoring/pause: checks keep running, but no points change hands. */
export function pauseScoring(send: AuthedFetch): Promise<Status> {
  return postJSON(send, '/scoring/pause');
}

/** POST /scoring/resume. */
export function resumeScoring(send: AuthedFetch): Promise<Status> {
  return postJSON(send, '/scoring/resume');
}

/**
 * POST /services/{name}/start. The routine failing to start rejects (a 500
 * whose message names the cause); the service stays enabled regardless.
 */
export function startService(send: AuthedFetch, name: string): Promise<StartResponse> {
  return postJSON(send, servicePath(name, 'start'));
}

/** POST /services/{name}/stop. */
export function stopService(send: AuthedFetch, name: string): Promise<Status> {
  return postJSON(send, servicePath(name, 'stop'));
}

/**
 * POST /services/{name}/restart, optionally pausing the service's scoring as
 * it comes back. Fails like startService.
 */
export function restartService(
  send: AuthedFetch,
  name: string,
  options: { pauseScoring?: boolean } = {},
): Promise<StartResponse> {
  const body = options.pauseScoring === undefined ? undefined : { pause_scoring: options.pauseScoring };
  return postJSON(send, servicePath(name, 'restart'), body);
}

/** POST /services/{name}/scoring/pause: pauses only this service's scoring. */
export function pauseServiceScoring(send: AuthedFetch, name: string): Promise<Status> {
  return postJSON(send, servicePath(name, 'scoring/pause'));
}

/** POST /services/{name}/scoring/resume. */
export function resumeServiceScoring(send: AuthedFetch, name: string): Promise<Status> {
  return postJSON(send, servicePath(name, 'scoring/resume'));
}

/**
 * POST /reload: re-reads the config file, adding and removing services.
 *
 * A reload that fails as a whole still changed what it got to before
 * failing, and its 500 says what that was, so that 500 resolves too, with
 * `error` set. Any other failure rejects.
 */
export async function reload(send: AuthedFetch): Promise<ReloadResponse> {
  const res = await send('/reload', postInit());
  if (res.ok) {
    return (await res.json()) as ReloadResponse;
  }
  const body = await jsonOrUndefined(res);
  if (isReloadResponse(body) && body.error) {
    return body;
  }
  throw new ApiError('POST', '/reload', res.status, errorMessageOf(body));
}

function isReloadResponse(body: unknown): body is ReloadResponse {
  return (
    typeof body === 'object' &&
    body !== null &&
    'added' in body &&
    Array.isArray(body.added) &&
    'removed' in body &&
    Array.isArray(body.removed) &&
    'failed' in body &&
    Array.isArray(body.failed)
  );
}

/**
 * POST /countdown: sets the time the scoreboard's countdown runs to, as an
 * RFC 3339 timestamp, or clears it with null. `target` is always sent, as an
 * explicit JSON null when clearing.
 */
export function setCountdown(send: AuthedFetch, target: string | null): Promise<Status> {
  return postJSON(send, '/countdown', { target });
}

async function getJSONAuthed<T>(send: AuthedFetch, path: string): Promise<T> {
  const res = await send(path, { headers: { Accept: 'application/json' } });
  if (!res.ok) {
    throw await responseError('GET', path, res);
  }
  return (await res.json()) as T;
}

/** The response of GET /config. */
export interface ConfigText {
  text: string;
  hash: string;
}

/** GET /config: the config file's current raw text and its hash. */
export function getConfig(send: AuthedFetch): Promise<ConfigText> {
  return getJSONAuthed(send, '/config');
}

/** The response of GET /config/status. */
export interface ConfigStatus {
  stale: boolean;
}

/**
 * GET /config/status: whether the file on disk differs from what the
 * engine last successfully loaded (i.e. a save is waiting on a reload).
 */
export function getConfigStatus(send: AuthedFetch): Promise<ConfigStatus> {
  return getJSONAuthed(send, '/config/status');
}

/** The response of POST /config. */
export interface SavedConfig {
  hash: string;
}

/**
 * POST /config: saves text as the new config file, rejecting with an
 * ApiError (status 409) if expectedHash no longer matches the file on disk
 * (someone else saved a newer version first), or 400 if text fails to
 * validate. Does not reload; that is still the separate POST /reload.
 */
export function saveConfig(send: AuthedFetch, text: string, expectedHash: string): Promise<SavedConfig> {
  return postJSON(send, '/config', { text, expected_hash: expectedHash });
}

/**
 * POST /teams/{side}/adjust: a manual, independent correction of side's
 * score by points (nonzero; positive adds, negative deducts). Rejects with
 * an ApiError (status 409) unless scoring is currently paused.
 */
export function adjustScore(send: AuthedFetch, side: 'blue' | 'red', points: number, reason: string): Promise<Status> {
  return postJSON(send, `/teams/${side}/adjust`, { points, reason });
}

/** The response of POST /backup: the current status plus the backup's path. */
export interface BackupResponse extends Status {
  backup_path: string;
}

/**
 * POST /backup: snapshots the database, rejecting with an ApiError (status
 * 409) unless the engine is fully stopped.
 */
export function backupDatabase(send: AuthedFetch): Promise<BackupResponse> {
  return postJSON(send, '/backup');
}
