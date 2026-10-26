import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { InjectView } from '../api/client';
import { TEAM_TOKEN_STORAGE_KEY } from '../admin/useAdminSession';
import { render } from '../test/render';
import { InjectsPage } from './InjectsPage';

const open: InjectView = {
  id: 1,
  team: 'blue',
  title: 'Firewall report',
  has_pdf: true,
  open: true,
  deadline: '2026-10-26T20:00:00Z',
  accepting: true,
  worth: 30000,
};

function jsonResponse(httpStatus: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status: httpStatus, headers: { 'Content-Type': 'application/json' } });
}

function stubApi(injects: InjectView[]) {
  const fetchMock = vi.fn<typeof fetch>(async (input, init) => {
    const key = `${init?.method ?? 'GET'} ${String(input)}`;
    if (key === 'GET /team/injects') return jsonResponse(200, injects);
    if (key === 'PUT /team/injects/1/submission') {
      return jsonResponse(200, {
        ...open,
        submission: { note: 'n', file_name: 'report.txt', size: 5, submitted_at: '2026-10-26T19:00:00Z' },
      });
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

describe('InjectsPage', () => {
  it('submits a note and a file to an open inject', async () => {
    const fetchMock = stubApi([open]);
    const user = userEvent.setup();
    render(<InjectsPage />);

    const row = await screen.findByRole('group', { name: 'Firewall report' });
    await user.type(within(row).getByLabelText('Note'), 'our answer');
    await user.upload(within(row).getByLabelText('File'), new File(['hello'], 'report.txt'));
    await user.click(within(row).getByRole('button', { name: 'Submit' }));

    expect(await within(row).findByText(/report\.txt/)).toBeInTheDocument();
    const put = fetchMock.mock.calls.find(([, init]) => init?.method === 'PUT');
    const body = put?.[1]?.body as FormData;
    expect(body.get('note')).toBe('our answer');
    expect((body.get('file') as File).name).toBe('report.txt');
  });

  it('shows an approved inject locked with its grade and points', async () => {
    stubApi([{ ...open, open: false, accepting: false, grade: 7, points: 21000, approved_at: '2026-10-26T21:00:00Z' }]);
    render(<InjectsPage />);

    const row = await screen.findByRole('group', { name: 'Firewall report' });
    expect(row).toHaveTextContent('7/10');
    expect(row).toHaveTextContent('21,000');
    expect(within(row).queryByRole('button', { name: 'Submit' })).toBeNull();
  });
});
