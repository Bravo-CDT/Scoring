import { Group, Text, UnstyledButton, VisuallyHidden } from '@mantine/core';
import type { CSSProperties } from 'react';
import type { ServiceStatus, Status } from '../api/client';
import { engineLogo, osLabel, osLogo, serviceLogo, type Logo } from './logos';
import classes from './NetworkDiagram.module.css';
import { StateBadge } from './StateBadge';
import { buildTopology, serviceHealth, type DiagramHost, type DiagramNetwork } from './topology';

/**
 * The scored network as a diagram: every network, every host on it with its
 * OS and address, and every service on each host with its state. Clicking a
 * service calls onSelectService with its name.
 */
export function NetworkDiagram({
  status,
  onSelectService,
}: {
  status: Status;
  onSelectService: (name: string) => void;
}) {
  const networks = buildTopology(status);
  if (networks.length === 0) {
    return <Text c="dimmed">No services configured.</Text>;
  }

  return (
    <div className={classes.diagram}>
      <div className={classes.engine}>
        <LogoImage logo={engineLogo} size={22} />
        <Text fw={700}>Scoring engine</Text>
      </div>
      <div className={classes.networks} style={{ '--network-count': networks.length } as CSSProperties}>
        {networks.map((network) => (
          <NetworkColumn key={network.key} network={network} onSelectService={onSelectService} />
        ))}
      </div>
    </div>
  );
}

function NetworkColumn({
  network,
  onSelectService,
}: {
  network: DiagramNetwork;
  onSelectService: (name: string) => void;
}) {
  return (
    <section className={classes.network} aria-label={network.cidr ?? 'Other hosts'}>
      <div className={classes.subnet}>
        <Group gap="xs" align="baseline" wrap="nowrap">
          <Text fw={700} ff="monospace" fz="lg">
            {network.cidr ?? 'Other hosts'}
          </Text>
          {network.name && <Text c="dimmed">{network.name}</Text>}
        </Group>
        <Text c={network.up === network.total ? 'green' : 'red'} fw={700} style={{ whiteSpace: 'nowrap' }}>
          {network.up}/{network.total} up
        </Text>
      </div>
      <div className={classes.hosts}>
        {network.hosts.map((host) => (
          <HostCard key={host.address} host={host} onSelectService={onSelectService} />
        ))}
      </div>
    </section>
  );
}

function HostCard({ host, onSelectService }: { host: DiagramHost; onSelectService: (name: string) => void }) {
  return (
    <article className={classes.host} data-health={host.health} aria-label={host.name ?? host.address}>
      <div className={classes.hostHeader}>
        <LogoImage logo={osLogo(host.os)} size={24} />
        <Text fw={700} fz="lg" ff={host.name ? undefined : 'monospace'} truncate>
          {host.name ?? host.address}
        </Text>
        {host.name && (
          <Text c="dimmed" ff="monospace">
            {host.address}
          </Text>
        )}
        {host.os && (
          <Text c="dimmed" fz="sm" ml="auto">
            {osLabel(host.os)}
          </Text>
        )}
      </div>
      <div className={classes.services}>
        {host.services.length === 0 ? (
          <Text c="dimmed" fz="sm">
            No scored services
          </Text>
        ) : (
          host.services.map((service) => (
            <ServiceRow key={service.name} service={service} onSelect={() => onSelectService(service.name)} />
          ))
        )}
      </div>
    </article>
  );
}

/** A service's logo, name, port and state; clicking it opens its history. */
function ServiceRow({ service, onSelect }: { service: ServiceStatus; onSelect: () => void }) {
  return (
    <UnstyledButton
      className={classes.service}
      data-health={serviceHealth(service)}
      onClick={onSelect}
      aria-haspopup="dialog"
    >
      <LogoImage logo={serviceLogo(service.name, service.check_type)} size={22} />
      <Text fw={600} className={classes.serviceName} truncate>
        {service.name}
      </Text>
      {service.port !== undefined && (
        <Text c="dimmed" ff="monospace" fz="sm">
          :{service.port}
        </Text>
      )}
      {service.checking && (
        <span className={classes.checking}>
          <VisuallyHidden>Scoring…</VisuallyHidden>
        </span>
      )}
      <StateBadge service={service} />
    </UnstyledButton>
  );
}

function LogoImage({ logo, size }: { logo: Logo; size: number }) {
  if (logo.mono) {
    return (
      <span
        className={classes.monoLogo}
        style={{ width: size, height: size, '--logo': `url("${logo.src}")` } as CSSProperties}
        aria-hidden
      />
    );
  }
  return <img className={classes.logo} src={logo.src} width={size} height={size} alt="" />;
}
