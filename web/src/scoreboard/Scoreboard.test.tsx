import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import * as client from '../api/client';
import { render } from '../test/render';
import { Scoreboard } from './Scoreboard';

const status: client.Status = {
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
    { name: 'dns', up: false, routine_enabled: true, scoring_paused: false, latency_ms: 0, consecutive_failures: 2 },
  ],
};

afterEach(() => {
  vi.restoreAllMocks();
});

describe('Scoreboard', () => {
  it("opens a service's history when its row is clicked, and closes it", async () => {
    vi.spyOn(client, 'getStatus').mockResolvedValue(status);
    const getResults = vi
      .spyOn(client, 'getResults')
      .mockResolvedValue([
        { id: 9, round: 5, timestamp: '2026-10-18T12:00:00Z', success: false, latency_ms: 0, detail: 'NXDOMAIN' },
      ]);
    const user = userEvent.setup();

    render(<Scoreboard />);
    await user.click(await screen.findByRole('button', { name: /dns/ }));

    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent('dns: recent checks');
    expect(await screen.findByText('NXDOMAIN')).toBeInTheDocument();
    expect(getResults).toHaveBeenCalledWith('dns', expect.any(Number));

    await user.click(screen.getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });
});
