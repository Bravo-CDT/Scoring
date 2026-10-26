import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import * as client from '../api/client';
import { render } from '../test/render';
import { Scoreboard } from './Scoreboard';

const status: client.Status = {
  session_name: 'demo',
  starting_score: 1000000,
  blue_score: 979000,
  red_score: 21000,
  engine_state: 'stopped',
  scoring_paused: true,
  server_time: '2026-10-26T12:00:00Z',
  round: 0,
  next_round_at: '0001-01-01T00:00:00Z',
  services: [],
  networks: [],
  hosts: [],
  credentials_enabled: false,
  injects_enabled: true,
};

const zero = { starting: 0, services: 0, injects: 0, manual: 0, scavenger_hunt: 0, total: 0 };

afterEach(() => {
  vi.restoreAllMocks();
});

describe('ScoreBreakdown', () => {
  it("opens a team's breakdown from its score", async () => {
    vi.spyOn(client, 'getStatus').mockResolvedValue(status);
    vi.spyOn(client, 'getScoreBreakdown').mockResolvedValue({
      blue: { ...zero, starting: 1000000, injects: -21000, total: 979000 },
      red: { ...zero, injects: 21000, total: 21000 },
    });
    const user = userEvent.setup();
    render(<Scoreboard />);

    await user.click(await screen.findByRole('button', { name: /Red/ }));
    const dialog = await screen.findByRole('dialog');
    expect(await within(dialog).findByRole('row', { name: /Injects/ })).toHaveTextContent('21,000');
    expect(within(dialog).getByRole('row', { name: /Scavenger hunt/ })).toHaveTextContent('0');
    expect(within(dialog).queryByRole('row', { name: /Starting score/ })).toBeNull();
  });
});
