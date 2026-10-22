import { describe, expect, it } from 'vitest';
import { MAX_DELAY_MS, nextPollDelayMs } from './useStatusPolling';

describe('nextPollDelayMs', () => {
  const now = new Date('2026-10-18T12:00:00Z');

  it('waits until 3s after a future next_round_at', () => {
    expect(nextPollDelayMs('2026-10-18T12:00:10Z', now)).toBe(13000);
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

  it('falls back to the normal schedule outside the window', () => {
    const nextRoundAt = '2026-10-18T12:00:30Z';
    const now = new Date('2026-10-18T12:00:00Z');
    expect(nextPollDelayMs(nextRoundAt, now, 30_000)).toBe(MAX_DELAY_MS);
  });
});
