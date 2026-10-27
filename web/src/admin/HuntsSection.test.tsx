import { fireEvent, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { AuthedFetch, HuntView } from '../api/client';
import { render } from '../test/render';
import { HuntsSection } from './HuntsSection';

const claimed: HuntView = {
  id: 1,
  team: 'either',
  title: 'Lobby flag',
  description: '',
  code: 'Flag{x}',
  points: 250,
  open: true,
  deadline: '2026-10-26T20:00:00Z',
  claimable: false,
  claimed_by: 'red',
  claimed_at: '2026-10-26T19:00:00Z',
  points_moved: 250,
};

function jsonResponse(httpStatus: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status: httpStatus, headers: { 'Content-Type': 'application/json' } });
}

function stubSend(hunts: HuntView[], handle: (key: string, init?: RequestInit) => Response | undefined) {
  return vi.fn<AuthedFetch>(async (path, init) => {
    const key = `${init?.method ?? 'GET'} ${path}`;
    if (key === 'GET /team/hunts') return jsonResponse(200, hunts);
    const res = handle(key, init);
    if (res) return res;
    throw new Error(`unexpected request: ${key}`);
  });
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe('HuntsSection', () => {
  it('creates a race hunt with points and a code', async () => {
    const send = stubSend([], (key) =>
      key === 'POST /admin/hunts'
        ? jsonResponse(200, { ...claimed, claimed_by: undefined, claimed_at: undefined, claimable: true })
        : undefined,
    );
    const user = userEvent.setup();
    render(<HuntsSection send={send} />);

    await user.type(screen.getByLabelText(/^Title/), 'Lobby flag');
    await user.selectOptions(screen.getByLabelText('Team'), 'either');
    await user.clear(screen.getByLabelText(/^Points/));
    await user.type(screen.getByLabelText(/^Points/), '250');
    await user.type(screen.getByLabelText(/^Code/), 'Flag{{x}');
    fireEvent.change(screen.getByLabelText(/^Deadline/), { target: { value: '2026-10-26T20:00' } });
    await user.click(screen.getByRole('button', { name: 'Create' }));

    expect(await screen.findByRole('group', { name: 'Lobby flag' })).toBeInTheDocument();
    const post = send.mock.calls.find(([, init]) => init?.method === 'POST');
    expect(JSON.parse(String(post?.[1]?.body))).toMatchObject({ team: 'either', points: 250, code: 'Flag{x}' });
  });

  it('locks a claimed hunt', async () => {
    const send = stubSend([claimed], () => undefined);
    render(<HuntsSection send={send} />);

    const card = await screen.findByRole('group', { name: 'Lobby flag' });
    expect(within(card).getByText('Claimed by red')).toBeInTheDocument();
    expect(within(card).getByRole('button', { name: 'Save' })).toBeDisabled();
    expect(within(card).getByRole('button', { name: 'Delete' })).toBeEnabled();
  });
});
