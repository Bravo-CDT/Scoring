import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Credentials } from '../api/client';
import { BLUE_TOKEN_STORAGE_KEY } from '../admin/useAdminSession';
import { render } from '../test/render';
import { CredentialsPage } from './CredentialsPage';

const credentials: Credentials = {
  allowed_users: ['mrkrabs', 'patrick'],
  credentials: [{ service: 'postgresql', host: '10.111.1.2', host_name: 'pattyvault', username: 'mrkrabs' }],
};

function jsonResponse(httpStatus: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status: httpStatus, headers: { 'Content-Type': 'application/json' } });
}

/** Stubs fetch: GET /blue/credentials answers `credentials`, PUT answers `put`. */
function stubApi(put: (init: RequestInit | undefined) => Response) {
  const fetchMock = vi.fn<typeof fetch>(async (input, init) => {
    const key = `${init?.method ?? 'GET'} ${String(input)}`;
    if (key === 'GET /blue/credentials') return jsonResponse(200, credentials);
    if (key === 'PUT /blue/credentials/postgresql') return put(init);
    throw new Error(`unexpected request: ${key}`);
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('CredentialsPage', () => {
  it('asks the blue team to log in first', () => {
    stubApi(() => jsonResponse(500, {}));
    render(<CredentialsPage />);
    expect(screen.getByRole('heading', { name: 'Blue team login' })).toBeInTheDocument();
  });

  it('lists each service with its box and current user', async () => {
    localStorage.setItem(BLUE_TOKEN_STORAGE_KEY, 'tok');
    stubApi(() => jsonResponse(500, {}));
    render(<CredentialsPage />);

    const row = await screen.findByRole('group', { name: 'postgresql' });
    expect(row).toHaveTextContent('mrkrabs');
    expect(row).toHaveTextContent('Default');
    expect(screen.getByText('pattyvault')).toBeInTheDocument();
  });

  it('sets a new user and password, then shows it changed', async () => {
    localStorage.setItem(BLUE_TOKEN_STORAGE_KEY, 'tok');
    const fetchMock = stubApi(() =>
      jsonResponse(200, { ...credentials.credentials[0], username: 'patrick', changed_at: '2026-10-26T14:02:00Z' }),
    );
    const user = userEvent.setup();
    render(<CredentialsPage />);

    const row = await screen.findByRole('group', { name: 'postgresql' });
    await user.selectOptions(within(row).getByLabelText('User'), 'patrick');
    const password = within(row).getByLabelText(/New password/);
    await user.type(password, 'Kelp42!');
    await user.click(within(row).getByRole('button', { name: 'Save' }));

    expect(await within(row).findByText(/Changed/)).toBeInTheDocument();
    const put = fetchMock.mock.calls.find(([, init]) => init?.method === 'PUT');
    expect(JSON.parse(String(put?.[1]?.body))).toEqual({ username: 'patrick', password: 'Kelp42!' });
    expect(new Headers(put?.[1]?.headers).get('Authorization')).toBe('Bearer tok');
    expect(password).toHaveValue('');
  });

  it("shows the API's reason when a change is refused", async () => {
    localStorage.setItem(BLUE_TOKEN_STORAGE_KEY, 'tok');
    stubApi(() => jsonResponse(400, { error: 'that user is not one of the allowed users' }));
    const user = userEvent.setup();
    render(<CredentialsPage />);

    const row = await screen.findByRole('group', { name: 'postgresql' });
    await user.type(within(row).getByLabelText(/New password/), 'x');
    await user.click(within(row).getByRole('button', { name: 'Save' }));

    expect(await within(row).findByText('that user is not one of the allowed users')).toBeInTheDocument();
  });
});
