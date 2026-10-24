import { describe, expect, it } from 'vitest';
import type { ServiceStatus, Status } from '../api/client';
import { buildTopology } from './topology';

function service(name: string, host: string, up: boolean | undefined): ServiceStatus {
  return {
    name,
    host,
    check_type: 'tcp_connect',
    up: up ?? false,
    routine_enabled: true,
    checking: false,
    scoring_paused: false,
    latency_ms: 0,
    consecutive_failures: 0,
    // No result yet when up is undefined.
    last_check_time: up === undefined ? undefined : '2026-10-18T12:00:00Z',
  };
}

const base: Status = {
  session_name: 's',
  starting_score: 1000,
  blue_score: 1000,
  red_score: 0,
  engine_state: 'running',
  scoring_paused: false,
  server_time: '2026-10-18T12:00:00Z',
  round: 1,
  next_round_at: '2026-10-18T12:01:00Z',
  services: [],
  networks: [],
  hosts: [],
};

describe('buildTopology', () => {
  it('groups services by host and hosts by network, in address order', () => {
    const networks = buildTopology({
      ...base,
      services: [
        service('ad', '10.111.2.1', false),
        service('dns', '10.111.2.1', true),
        service('web', '10.111.1.10', true),
        service('db', '10.111.1.2', true),
        service('mail', 'mail.example', undefined),
      ],
      networks: [{ cidr: '10.111.2.0/24', name: 'windows' }],
      hosts: [
        { address: '10.111.2.1', name: 'dc', os: 'windows' },
        { address: '10.111.2.9', name: 'spare' },
      ],
    });

    expect(networks.map((n) => [n.key, n.name, n.up, n.total])).toEqual([
      ['10.111.1.0/24', undefined, 2, 2],
      ['10.111.2.0/24', 'windows', 1, 2],
      ['other', undefined, 0, 1],
    ]);
    // Numeric, not string, address order: .2 before .10.
    expect(networks[0].hosts.map((h) => h.address)).toEqual(['10.111.1.2', '10.111.1.10']);
    const [dc, spare] = networks[1].hosts;
    expect(dc).toMatchObject({ name: 'dc', os: 'windows', health: 'down' });
    expect(dc.services.map((s) => s.name)).toEqual(['ad', 'dns']);
    // A configured host with nothing scored on it still shows.
    expect(spare).toMatchObject({ name: 'spare', services: [], health: 'unknown' });
    expect(networks[2].hosts[0].health).toBe('unknown');
  });

  it('puts a host under the most specific configured network containing it', () => {
    const networks = buildTopology({
      ...base,
      services: [service('a', '10.1.2.3', true)],
      networks: [
        { cidr: '10.0.0.0/8', name: 'wide' },
        { cidr: '10.1.0.0/16', name: 'narrow' },
      ],
    });
    expect(networks.map((n) => n.name)).toEqual(['narrow']);
  });
});
