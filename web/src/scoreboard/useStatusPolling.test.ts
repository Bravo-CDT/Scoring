import { describe, expect, it } from 'vitest';
import { nextPollDelayMs } from './useStatusPolling';

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

  it('falls back to 5s when next_round_at is exactly now', () => {
    expect(nextPollDelayMs('2026-10-18T12:00:00Z', now)).toBe(5000);
  });

  it('falls back to 5s for an unparseable value', () => {
    expect(nextPollDelayMs('not a time', now)).toBe(5000);
  });

  it('clamps an absurdly far-future value to 30s', () => {
    expect(nextPollDelayMs('2026-10-18T13:00:00Z', now)).toBe(30000);
  });

  it('handles sub-second precision as Go encodes it', () => {
    expect(nextPollDelayMs('2026-10-18T12:00:02.5+00:00', now)).toBe(5500);
  });
});
