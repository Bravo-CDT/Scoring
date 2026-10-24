import { describe, expect, it } from 'vitest';
import { MAX_DELAY_MS, nextPollDelayMs, withCheckingOverride } from './useStatusPolling';

describe('nextPollDelayMs', () => {
  const now = new Date('2026-10-18T12:00:00Z');

  it('aims to land the next poll right as the burst window opens', () => {
    // 10s away, default (unscaled) 4s window: lands 6s from now, at the
    // window's edge, not 3s past the round the way the old settle-clamp
    // formula did -- that always overshot into the *next* round's own
    // burst window by the time the response came back (see the
    // "steady-state" tests below), so the live Scoring tag was almost
    // never seen at realistic round lengths.
    expect(nextPollDelayMs('2026-10-18T12:00:10Z', now)).toBe(6000);
  });

  it('falls back to 5s for the zero-time value', () => {
    expect(nextPollDelayMs('0001-01-01T00:00:00Z', now)).toBe(5000);
  });

  it('falls back to 5s for a stale past value', () => {
    expect(nextPollDelayMs('2026-10-18T11:59:00Z', now)).toBe(5000);
  });

  it('bursts at 500ms when next_round_at is exactly now (the center of the burst window)', () => {
    expect(nextPollDelayMs('2026-10-18T12:00:00Z', now)).toBe(500);
  });

  it('falls back to 5s for an unparseable value', () => {
    expect(nextPollDelayMs('not a time', now)).toBe(5000);
  });

  it('clamps an absurdly far-future value to 30s', () => {
    expect(nextPollDelayMs('2026-10-18T13:00:00Z', now)).toBe(30000);
  });

  it('bursts at 500ms within the window instead of the sub-second settle-clamp delay', () => {
    // 2.5s before the round is due: inside the default (unscaled) 4s burst
    // window, which now takes priority over the old +3s/clamp calculation.
    expect(nextPollDelayMs('2026-10-18T12:00:02.5+00:00', now)).toBe(500);
  });
});

describe('nextPollDelayMs steady state', () => {
  it('eventually polls inside the burst window every round, for realistic round lengths', () => {
    // Simulates the schedule nextPollDelayMs alone would produce across many
    // rounds, the way useStatusPolling actually drives it: at any instant
    // `now`, the reported next_round_at is whichever boundary is next
    // strictly after now (mirroring the scheduler's early-publish fix,
    // which makes next_round_at jump the moment a round begins), and each
    // poll's delay is computed from that. Before this fix (the old
    // +3s-after-due formula), a poll timed to land 3s after one boundary
    // would, by the time it fetched, already see the *next* boundary
    // reported -- putting it a near-full round length away from what it
    // just learned, so the schedule drifted forever and a burst poll
    // essentially never happened past the first round or two.
    for (const roundLengthMs of [30_000, 60_000]) {
      let now = 0;
      let sawBurst = false;
      const totalSimulatedMs = 15 * roundLengthMs;
      for (let i = 0; i < 1000 && now < totalSimulatedMs; i++) {
        const nextRoundAtMs = roundLengthMs * (Math.floor(now / roundLengthMs) + 1);
        const delay = nextPollDelayMs(new Date(nextRoundAtMs).toISOString(), new Date(now), roundLengthMs);
        if (delay === 500) sawBurst = true;
        now += delay;
      }
      expect(sawBurst, `round length ${roundLengthMs}ms never saw a burst poll across 15 rounds`).toBe(true);
    }
  });
});

describe('withCheckingOverride', () => {
  it('overrides the delay to the burst interval when any service is checking', () => {
    expect(withCheckingOverride(30_000, [{ checking: false }, { checking: true }])).toBe(500);
  });

  it('leaves the delay alone when nothing is checking', () => {
    expect(withCheckingOverride(30_000, [{ checking: false }, { checking: false }])).toBe(30_000);
  });

  it('leaves the delay alone for no services', () => {
    expect(withCheckingOverride(30_000, [])).toBe(30_000);
  });
});

describe('nextPollDelayMs burst window', () => {
  it('polls every 500ms shortly before a round is due', () => {
    const nextRoundAt = '2026-10-18T12:00:10Z';
    const now = new Date('2026-10-18T12:00:07.000Z'); // 3s before, inside a 4s window
    expect(nextPollDelayMs(nextRoundAt, now, 30_000)).toBe(500);
  });

  it('polls every 500ms shortly after a round was due', () => {
    const nextRoundAt = '2026-10-18T12:00:10Z';
    const now = new Date('2026-10-18T12:00:12.000Z'); // 2s after
    expect(nextPollDelayMs(nextRoundAt, now, 30_000)).toBe(500);
  });

  it('scales the window down for a short round interval instead of bursting continuously', () => {
    const nextRoundAt = '2026-10-18T12:00:05Z';
    const now = new Date('2026-10-18T12:00:03.300Z'); // 1.7s before a 5s round: outside a 1.5s window
    expect(nextPollDelayMs(nextRoundAt, now, 5_000)).not.toBe(500);
  });

  it('clamps to the max delay for a round far enough away', () => {
    const nextRoundAt = '2026-10-18T12:01:00Z'; // 60s away: minus the 4s window still clamps to 30s
    const now = new Date('2026-10-18T12:00:00Z');
    expect(nextPollDelayMs(nextRoundAt, now, 30_000)).toBe(MAX_DELAY_MS);
  });
});
