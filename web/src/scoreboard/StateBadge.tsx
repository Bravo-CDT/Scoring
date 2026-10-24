import { Badge } from '@mantine/core';
import type { ServiceStatus } from '../api/client';

/**
 * A service's up/down state. A service with no check result yet (just added,
 * or right after a reset) is "Unregistered" rather than "Down": nothing is
 * known about it yet.
 */
export function StateBadge({ service }: { service: ServiceStatus }) {
  if (!service.last_check_time) {
    return (
      <Badge color="gray" variant="filled">
        Unregistered
      </Badge>
    );
  }
  return (
    <Badge color={service.up ? 'green' : 'red'} variant="filled">
      {service.up ? 'Up' : 'Down'}
    </Badge>
  );
}
