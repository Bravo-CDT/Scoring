import type { ServiceStatus, Status } from '../api/client';

/** What a service, host or network looks like on the diagram. */
export type Health = 'up' | 'down' | 'unknown';

/** One machine on the diagram, with the services checked on it. */
export interface DiagramHost {
  address: string;
  name?: string;
  os?: string;
  services: ServiceStatus[];
  health: Health;
}

/** One subnet on the diagram, with its hosts in address order. */
export interface DiagramNetwork {
  /** Unique per network: its CIDR, or "other" for hosts outside any. */
  key: string;
  cidr?: string;
  name?: string;
  hosts: DiagramHost[];
  /** How many of its services are up, of how many in all. */
  up: number;
  total: number;
}

/** A service's state: unknown until its first check has a result. */
export function serviceHealth(service: ServiceStatus): Health {
  if (!service.last_check_time) return 'unknown';
  return service.up ? 'up' : 'down';
}

/** Down if any of them is down, unknown if none has a result yet, else up. */
function combinedHealth(services: ServiceStatus[]): Health {
  const healths = services.map(serviceHealth);
  if (healths.includes('down')) return 'down';
  if (healths.includes('up')) return 'up';
  return 'unknown';
}

/** An IPv4 address as an unsigned 32-bit number, or undefined if `s` isn't one. */
export function parseIPv4(s: string): number | undefined {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s);
  if (!m) return undefined;
  const octets = m.slice(1).map(Number);
  if (octets.some((o) => o > 255)) return undefined;
  return octets.reduce((n, o) => n * 256 + o, 0);
}

interface Prefix {
  base: number;
  bits: number;
}

function parseCIDR(cidr: string): Prefix | undefined {
  const [addr, bitsText] = cidr.split('/');
  const ip = parseIPv4(addr);
  const bits = Number(bitsText);
  if (ip === undefined || !Number.isInteger(bits) || bits < 0 || bits > 32) return undefined;
  return { base: masked(ip, bits), bits };
}

function masked(ip: number, bits: number): number {
  return bits === 0 ? 0 : (ip & ((~0 << (32 - bits)) >>> 0)) >>> 0;
}

function formatIPv4(n: number): string {
  return [24, 16, 8, 0].map((shift) => (n >>> shift) & 255).join('.');
}

/**
 * Groups the status's services into the network diagram: services by host,
 * hosts by network. A host goes under the most specific configured network
 * that contains it, else under its own /24; a host that isn't an IPv4
 * address goes under "other", last. Configured hosts with no services still
 * appear, so the diagram shows every machine named in the config.
 */
export function buildTopology(status: Status): DiagramNetwork[] {
  const servicesByHost = new Map<string, ServiceStatus[]>();
  for (const service of status.services) {
    const list = servicesByHost.get(service.host) ?? [];
    list.push(service);
    servicesByHost.set(service.host, list);
  }
  for (const host of status.hosts) {
    if (!servicesByHost.has(host.address)) servicesByHost.set(host.address, []);
  }

  const hostInfo = new Map(status.hosts.map((h) => [h.address, h]));
  const configured = status.networks
    .map((n) => ({ network: n, prefix: parseCIDR(n.cidr) }))
    .filter((n): n is { network: (typeof status.networks)[number]; prefix: Prefix } => n.prefix !== undefined)
    // Most specific first, so the first match is the best one.
    .sort((a, b) => b.prefix.bits - a.prefix.bits);

  const networks = new Map<string, DiagramNetwork & { sortKey: number }>();
  for (const [address, services] of servicesByHost) {
    const ip = parseIPv4(address);
    let key = 'other';
    let cidr: string | undefined;
    let name: string | undefined;
    let sortKey = Infinity;
    if (ip !== undefined) {
      const match = configured.find((c) => masked(ip, c.prefix.bits) === c.prefix.base);
      const prefix = match?.prefix ?? { base: masked(ip, 24), bits: 24 };
      cidr = `${formatIPv4(prefix.base)}/${prefix.bits}`;
      key = cidr;
      name = match?.network.name;
      sortKey = prefix.base;
    }
    const network = networks.get(key) ?? { key, cidr, name, hosts: [], up: 0, total: 0, sortKey };
    const info = hostInfo.get(address);
    const sorted = [...services].sort((a, b) => a.name.localeCompare(b.name));
    network.hosts.push({ address, name: info?.name, os: info?.os, services: sorted, health: combinedHealth(sorted) });
    network.up += sorted.filter((s) => serviceHealth(s) === 'up').length;
    network.total += sorted.length;
    networks.set(key, network);
  }

  const byAddress = (a: DiagramHost, b: DiagramHost) =>
    (parseIPv4(a.address) ?? Infinity) - (parseIPv4(b.address) ?? Infinity) || a.address.localeCompare(b.address);
  return [...networks.values()]
    .sort((a, b) => a.sortKey - b.sortKey)
    .map(({ key, cidr, name, hosts, up, total }) => ({ key, cidr, name, hosts: hosts.sort(byAddress), up, total }));
}
