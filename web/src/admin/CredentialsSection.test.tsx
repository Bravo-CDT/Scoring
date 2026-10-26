import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { AuthedFetch, Credentials } from '../api/client';
import { render } from '../test/render';
import { CredentialsSection } from './CredentialsSection';

const changed: Credentials = {
  allowed_users: ['mrkrabs', 'patrick'],
  credentials: [
    {
      service: 'postgresql',
      host: '10.111.1.2',
      host_name: 'pattyvault',
      username: 'patrick',
      changed_at: '2026-10-26T14:02:00Z',
    },
  ],
};

function jsonResponse(httpStatus: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status: httpStatus, headers: { 'Content-Type': 'application/json' } });
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe('CredentialsSection', () => {
  it('resets a changed service to its default after confirming', async () => {
    let current = changed;
    const send = vi.fn<AuthedFetch>(async (path, init) => {
      const key = `${init?.method ?? 'GET'} ${path}`;
      if (key === 'GET /blue/credentials') return jsonResponse(200, current);
      if (key === 'POST /blue/credentials/postgresql/reset') {
        const entry = { service: 'postgresql', host: '10.111.1.2', host_name: 'pattyvault', username: 'mrkrabs' };
        current = { ...changed, credentials: [entry] };
        return jsonResponse(200, entry);
      }
      throw new Error(`unexpected request: ${key}`);
    });
    const user = userEvent.setup();
    render(<CredentialsSection send={send} />);

    const row = await screen.findByRole('row', { name: /postgresql/ });
    expect(row).toHaveTextContent('patrick');
    expect(row).toHaveTextContent('Changed');

    await user.click(within(row).getByRole('button', { name: 'Reset postgresql to default' }));
    expect(send).not.toHaveBeenCalledWith('/blue/credentials/postgresql/reset', expect.anything());
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: 'Reset to default' }));

    const updated = await screen.findByRole('row', { name: /postgresql/ });
    expect(await within(updated).findByText('Default')).toBeInTheDocument();
    expect(updated).toHaveTextContent('mrkrabs');
  });
});
