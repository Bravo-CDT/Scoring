import { useEffect, useRef, useState } from 'react';
import { getStatus, type Status } from '../api/client';

const MIN_DELAY_MS = 1_000;
export const MAX_DELAY_MS = 30_000;
/** The delay whenever next_round_at gives nothing to go on, and after a failed fetch. */
export const FALLBACK_DELAY_MS = 5_000;

/** How often to poll inside the burst window around a round boundary. */
const BURST_INTERVAL_MS = 500;
/** The widest the burst window can be, on each side of next_round_at. */
const MAX_BURST_HALF_WIDTH_MS = 4_000;
/**
 * The fraction of an observed round length the burst window is capped at,
 * so a short round (e.g. a demo config's 5s) doesn't spend nearly all its
 * time bursting -- the anti-goal the fixed 4s width would otherwise hit.
 */
const BURST_WINDOW_FRACTION = 0.3;

/**
 * Half the width of the burst-polling window around next_round_at: the
 * smaller of a flat 4s and 30% of roundLengthMs.
 */
function burstWindowMs(roundLengthMs: number): number {
  return Math.min(MAX_BURST_HALF_WIDTH_MS, BURST_WINDOW_FRACTION * roundLengthMs);
}

/**
 * How long to wait before polling /status again, given the `next_round_at`
 * it last reported.
 *
 * Inside the burst window around next_round_at (its width set by
 * roundLengthMs, the caller's best estimate of the round length -- omit it,
 * or pass MAX_DELAY_MS, for the widest 4s window), polls every 500ms so the
 * live "Scoring…" tag doesn't get missed between polls timed for later.
 *
 * Outside that window, aims to land the *next* poll right as the window
 * opens (untilRound - the window's width), clamped to [1s, 30s] -- not
 * shortly after the round is due, which only reads the window correctly if
 * the fetch happens to land strictly before the round begins. Because the
 * scheduler publishes next_round_at the instant a round begins (not once it
 * finishes), a poll timed to land *after* one boundary already sees the
 * *next* boundary by the time it fetches, so aiming "after" drifts a full
 * round length away from the window every single round and the window is
 * essentially never reached in steady state. Landing "before" it only ever
 * needs one correction to get in sync, however far off the starting point.
 *
 * A `next_round_at` that is not in the future (and outside the burst
 * window) gives no usable prediction, and falls back to a fixed 5s rather
 * than a zero or negative delay that would poll in a tight loop. That
 * covers Go's zero time (0001-01-01T00:00:00Z, before the engine's first
 * round) and a stale past value (nothing enabled, or a slow check that made
 * a round land later than predicted) — both deliberate backend behaviors.
 */
export function nextPollDelayMs(nextRoundAt: string, now: Date, roundLengthMs = MAX_DELAY_MS): number {
  const untilRound = Date.parse(nextRoundAt) - now.getTime();
  const window = burstWindowMs(roundLengthMs);
  // NaN (unparseable) fails this comparison too, falling through below.
  if (Math.abs(untilRound) <= window) {
    return BURST_INTERVAL_MS;
  }
  if (!(untilRound > 0)) {
    return FALLBACK_DELAY_MS;
  }
  return Math.min(Math.max(untilRound - window, MIN_DELAY_MS), MAX_DELAY_MS);
}

/**
 * Overrides delay with the burst interval whenever any service in the
 * latest status has its check in flight right now, so a check that runs
 * longer than the burst window (nothing bounds how long a check can take
 * beyond the round interval itself) doesn't fall back to the normal
 * schedule while it's still running -- this is also what catches the
 * "just after the boundary" half of the window, now that the window
 * itself only ever reaches up to the boundary (see nextPollDelayMs).
 */
export function withCheckingOverride(delay: number, services: { checking: boolean }[]): number {
  return services.some((s) => s.checking) ? BURST_INTERVAL_MS : delay;
}

export interface StatusPolling {
  /** The latest status fetched; null until the first fetch succeeds. */
  status: Status | null;
  /** Why the latest fetch failed; null once one succeeds again. */
  error: Error | null;
  /**
   * The engine host's clock minus this browser's, measured when `status`
   * arrived. Add it to Date.now() to get the engine's idea of now.
   */
  serverClockOffsetMs: number;
}

/**
 * Polls GET /status for as long as the calling component is mounted: once
 * immediately, then again shortly after each round is due (see
 * nextPollDelayMs). A failed fetch keeps the last good status, reports the
 * error, and retries after the fallback delay, so polling never stops.
 */
export function useStatusPolling(): StatusPolling {
  const [state, setState] = useState<StatusPolling>({
    status: null,
    error: null,
    serverClockOffsetMs: 0,
  });
  // The gap between the last two distinct next_round_at values seen, our
  // running estimate of the round length for sizing the burst window (see
  // nextPollDelayMs). Defaults to the widest window until a second value
  // arrives to measure a gap from.
  const roundLengthMs = useRef(MAX_DELAY_MS);
  const lastNextRoundAtMs = useRef<number | null>(null);

  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;

    async function poll() {
      let delay = FALLBACK_DELAY_MS;
      try {
        const status = await getStatus();
        if (cancelled) return;
        const serverNow = new Date(status.server_time);
        const nextMs = Date.parse(status.next_round_at);
        if (Number.isFinite(nextMs)) {
          if (lastNextRoundAtMs.current !== null) {
            const observed = nextMs - lastNextRoundAtMs.current;
            if (observed > 0) {
              roundLengthMs.current = observed;
            }
          }
          lastNextRoundAtMs.current = nextMs;
        }
        // Measure the delay against the engine's own clock, so a skewed
        // browser clock cannot shift when this polls relative to rounds.
        delay = withCheckingOverride(
          nextPollDelayMs(status.next_round_at, serverNow, roundLengthMs.current),
          status.services,
        );
        const offset = serverNow.getTime() - Date.now();
        setState({
          status,
          error: null,
          serverClockOffsetMs: Number.isNaN(offset) ? 0 : offset,
        });
      } catch (err) {
        if (cancelled) return;
        const error = err instanceof Error ? err : new Error(String(err));
        setState((prev) => ({ ...prev, error }));
      }
      timer = setTimeout(poll, delay);
    }

    void poll();
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, []);

  return state;
}
