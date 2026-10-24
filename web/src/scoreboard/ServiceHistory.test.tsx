import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import * as client from '../api/client';
import { render } from '../test/render';
import { ServiceHistory } from './ServiceHistory';

afterEach(() => {
  vi.restoreAllMocks();
});

describe('ServiceHistory', () => {
  it('renders detail text literally, never as HTML', async () => {
    const detail = '<script>alert(1)</script><img src=x onerror=alert(2)> connection refused';
    vi.spyOn(client, 'getResults').mockResolvedValue([
      { id: 1, round: 3, timestamp: '2026-10-18T12:00:00Z', success: false, latency_ms: 0, detail },
    ]);

    render(<ServiceHistory name="web" onClose={() => {}} />);

    // The whole string, markup-shaped substrings included, is the element's
    // visible text: it was never parsed as HTML...
    const shown = await screen.findByText(/connection refused/);
    expect(shown).toHaveTextContent(detail, { normalizeWhitespace: false });
    expect(shown.children).toHaveLength(0);
    // ...so none of the elements it spells out exist in the DOM.
    expect(document.querySelector('script')).toBeNull();
    expect(document.querySelector('img')).toBeNull();
  });

  it('lists results in the order the API returns them (newest first), with round and outcome', async () => {
    vi.spyOn(client, 'getResults').mockResolvedValue([
      { id: 12, round: 7, timestamp: '2026-10-18T12:01:00Z', success: true, latency_ms: 42 },
      { id: 11, round: 6, timestamp: '2026-10-18T12:00:30Z', success: false, latency_ms: 0, detail: 'timeout' },
    ]);

    render(<ServiceHistory name="web" onClose={() => {}} />);

    const items = await screen.findAllByRole('listitem');
    expect(items).toHaveLength(2);
    expect(within(items[0]).getByText('Round 7')).toBeInTheDocument();
    expect(within(items[0]).getByText('Up')).toBeInTheDocument();
    expect(within(items[1]).getByText('Round 6')).toBeInTheDocument();
    expect(within(items[1]).getByText('Down')).toBeInTheDocument();
    expect(within(items[1]).getByText('timeout')).toBeInTheDocument();
  });

  it('fetches the named service', async () => {
    const getResults = vi.spyOn(client, 'getResults').mockResolvedValue([]);

    render(<ServiceHistory name="mail server" onClose={() => {}} />);

    expect(await screen.findByText(/no checks yet/i)).toBeInTheDocument();
    expect(getResults).toHaveBeenCalledWith('mail server', expect.any(Number));
  });

  it('shows an error when the history cannot be fetched', async () => {
    vi.spyOn(client, 'getResults').mockRejectedValue(new Error('GET /services/web/results: 500'));

    render(<ServiceHistory name="web" onClose={() => {}} />);

    expect(await screen.findByText(/GET \/services\/web\/results: 500/)).toBeInTheDocument();
  });

  it('calls onClose when closed', async () => {
    vi.spyOn(client, 'getResults').mockResolvedValue([]);
    const onClose = vi.fn();

    render(<ServiceHistory name="web" onClose={onClose} />);
    await screen.findByText(/no checks yet/i);
    await userEvent.click(screen.getByRole('button', { name: /close/i }));

    expect(onClose).toHaveBeenCalled();
  });
});
