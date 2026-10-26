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
    {
      name: 'dns',
      up: false,
      routine_enabled: true,
      checking: false,
      scoring_paused: false,
      latency_ms: 0,
      consecutive_failures: 2,
      host: '10.0.0.1',
      check_type: 'tcp_connect',
    },
  ],
  networks: [],
  hosts: [],
  credentials_enabled: false,
};

afterEach(() => {
  vi.restoreAllMocks();
});

describe('Scoreboard', () => {
  it('shows a countdown to the next round while the engine is running', async () => {
    vi.spyOn(client, 'getStatus').mockResolvedValue(status);
    render(<Scoreboard />);
    expect(await screen.findByText('Next round in')).toBeInTheDocument();
  });

  it('hides the round countdown when the engine is stopped', async () => {
    vi.spyOn(client, 'getStatus').mockResolvedValue({ ...status, engine_state: 'stopped' });
    render(<Scoreboard />);
    await screen.findByText(status.session_name);
    expect(screen.queryByText('Next round in')).not.toBeInTheDocument();
  });

  it('links to the admin console', async () => {
    vi.spyOn(client, 'getStatus').mockResolvedValue(status);
    render(<Scoreboard />);
    expect(await screen.findByRole('link', { name: 'Admin' })).toHaveAttribute('href', '/admin');
  });

  it('links to the credentials page only while it is on', async () => {
    vi.spyOn(client, 'getStatus').mockResolvedValue({ ...status, credentials_enabled: true });
    const { unmount } = render(<Scoreboard />);
    expect(await screen.findByRole('link', { name: 'Credentials' })).toHaveAttribute('href', '/credentials');
    unmount();

    vi.spyOn(client, 'getStatus').mockResolvedValue(status);
    render(<Scoreboard />);
    await screen.findByRole('link', { name: 'Admin' });
    expect(screen.queryByRole('link', { name: 'Credentials' })).not.toBeInTheDocument();
  });

  it('shows a Scoring badge while a service check is in flight', async () => {
    vi.spyOn(client, 'getStatus').mockResolvedValue({
      ...status,
      services: [{ ...status.services[0], checking: true }, { ...status.services[1], checking: false }],
    });
    render(<Scoreboard />);
    expect(await screen.findByText('Scoring…')).toBeInTheDocument();
  });

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
