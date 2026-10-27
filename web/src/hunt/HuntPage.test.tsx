import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { HuntView } from '../api/client';
import { TEAM_TOKEN_STORAGE_KEY } from '../admin/useAdminSession';
import { render } from '../test/render';
import { HuntPage } from './HuntPage';

const hunt: HuntView = {
  id: 1,
  team: 'either',
  title: 'Find the flag',
  description: '',
  points: 500,
  open: true,
  deadline: '2026-10-26T20:00:00Z',
  claimable: true,
};

function jsonResponse(httpStatus: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status: httpStatus, headers: { 'Content-Type': 'application/json' } });
}

function stubApi(hunts: HuntView[]) {
  const fetchMock = vi.fn<typeof fetch>(async (input, init) => {
    const key = `${init?.method ?? 'GET'} ${String(input)}`;
    if (key === 'GET /team/hunts') return jsonResponse(200, hunts);
    if (key === 'POST /team/hunts/1/claim') {
      return jsonResponse(200, { ...hunt, claimable: false, claimed_by: 'blue', points_moved: 500 });
    }
    throw new Error(`unexpected request: ${key}`);
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

beforeEach(() => {
  localStorage.clear();
  localStorage.setItem(TEAM_TOKEN_STORAGE_KEY, 'tok');
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('HuntPage', () => {
  it('claims a hunt with the code typed exactly', async () => {
    const fetchMock = stubApi([hunt]);
    const user = userEvent.setup();
    render(<HuntPage />);

    await screen.findByRole('group', { name: 'Find the flag' });
    await user.selectOptions(screen.getByLabelText('Hunt'), '1');
    await user.type(screen.getByLabelText('Code'), ' Abc ');
    await user.click(screen.getByRole('button', { name: 'Submit' }));

    const row = await screen.findByRole('group', { name: 'Find the flag' });
    expect(await within(row).findByText('Claimed by blue')).toBeInTheDocument();
    const post = fetchMock.mock.calls.find(([, init]) => init?.method === 'POST');
    expect(post?.[1]?.body).toBe('{"code":" Abc "}');
  });

  it('offers only claimable hunts in the dropdown', async () => {
    stubApi([hunt, { ...hunt, id: 2, title: 'Taken', claimable: false, claimed_by: 'red' }]);
    render(<HuntPage />);

    await screen.findByRole('group', { name: 'Taken' });
    expect(within(screen.getByLabelText('Hunt')).getAllByRole('option')).toHaveLength(2);
    expect(screen.getByRole('group', { name: 'Find the flag' })).toBeInTheDocument();
  });
});
