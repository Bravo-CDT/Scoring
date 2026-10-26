import { fireEvent, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { AuthedFetch, InjectView } from '../api/client';
import { render } from '../test/render';
import { InjectsSection } from './InjectsSection';

const graded: InjectView = {
  id: 1,
  team: 'red',
  title: 'Phishing report',
  has_pdf: false,
  open: false,
  deadline: '2026-10-26T20:00:00Z',
  accepting: false,
  worth: 30000,
  grade: 7,
  submission: { note: 'done', file_name: 'r.txt', size: 3, submitted_at: '2026-10-26T19:00:00Z' },
};

function jsonResponse(httpStatus: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status: httpStatus, headers: { 'Content-Type': 'application/json' } });
}

function stubSend(injects: InjectView[], handle: (key: string, init?: RequestInit) => Response | undefined) {
  return vi.fn<AuthedFetch>(async (path, init) => {
    const key = `${init?.method ?? 'GET'} ${path}`;
    if (key === 'GET /team/injects') return jsonResponse(200, injects);
    const res = handle(key, init);
    if (res) return res;
    throw new Error(`unexpected request: ${key}`);
  });
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe('InjectsSection', () => {
  it('creates an inject with its title, team and deadline', async () => {
    const send = stubSend([], (key) =>
      key === 'POST /admin/injects' ? jsonResponse(200, { ...graded, grade: undefined, submission: undefined }) : undefined,
    );
    const user = userEvent.setup();
    render(<InjectsSection send={send} />);

    await user.type(screen.getByLabelText(/^Title/), 'Phishing report');
    await user.selectOptions(screen.getByLabelText('Team'), 'red');
    fireEvent.change(screen.getByLabelText(/^Deadline/), { target: { value: '2026-10-26T20:00' } });
    await user.click(screen.getByRole('button', { name: 'Create' }));

    expect(await screen.findByRole('group', { name: 'Phishing report' })).toBeInTheDocument();
    const post = send.mock.calls.find(([, init]) => init?.method === 'POST');
    expect(JSON.parse(String(post?.[1]?.body))).toEqual({
      title: 'Phishing report',
      team: 'red',
      deadline: new Date('2026-10-26T20:00').toISOString(),
    });
  });

  it('approves a graded inject after confirming', async () => {
    const send = stubSend([graded], (key) =>
      key === 'POST /admin/injects/1/approve'
        ? jsonResponse(200, { ...graded, points: 21000, approved_at: '2026-10-26T21:00:00Z' })
        : undefined,
    );
    const user = userEvent.setup();
    render(<InjectsSection send={send} />);

    const row = await screen.findByRole('group', { name: 'Phishing report' });
    await user.click(within(row).getByRole('button', { name: /Approve/ }));
    expect(send).not.toHaveBeenCalledWith('/admin/injects/1/approve', expect.anything());
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent('21,000');
    await user.click(within(dialog).getByRole('button', { name: 'Approve' }));

    expect(await within(row).findByText('Approved')).toBeInTheDocument();
    expect(send).toHaveBeenCalledWith('/admin/injects/1/approve', expect.anything());
  });
});
