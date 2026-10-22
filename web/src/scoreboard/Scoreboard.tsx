import {
  Alert,
  Badge,
  Button,
  Center,
  Container,
  Group,
  Loader,
  Paper,
  SimpleGrid,
  Stack,
  Text,
  Title,
  UnstyledButton,
} from '@mantine/core';
import { useEffect, useState } from 'react';
import type { ServiceStatus, Status } from '../api/client';
import { ServiceHistory } from './ServiceHistory';
import { useStatusPolling } from './useStatusPolling';

/** The public scoreboard: scores, services' up/down state, and the countdown. */
export function Scoreboard() {
  const { status, error, serverClockOffsetMs } = useStatusPolling();
  // The service whose history is open, if any.
  const [historyOf, setHistoryOf] = useState<string | null>(null);

  if (status === null) {
    return (
      <Center h="100vh">
        <Stack align="center">
          <Loader />
          <Text c="dimmed">
            {error ? `Cannot reach the scoring engine (${error.message}); retrying…` : 'Loading scoreboard…'}
          </Text>
        </Stack>
      </Center>
    );
  }

  return (
    <Container size="md" py="xl">
      <Stack gap="lg">
        <Group justify="space-between" align="baseline">
          <Title order={1}>{status.session_name}</Title>
          <Group gap="md" align="baseline">
            <Freshness status={status} />
            <Button component="a" href="/admin" variant="subtle" size="xs">
              Admin
            </Button>
          </Group>
        </Group>

        {error && (
          <Alert color="yellow" title="Connection lost">
            Showing the last update received; retrying. ({error.message})
          </Alert>
        )}

        <SimpleGrid cols={2}>
          <TeamScore team="Blue" color="blue" score={status.blue_score} />
          <TeamScore team="Red" color="red" score={status.red_score} />
        </SimpleGrid>

        {status.engine_state === 'running' && (
          <Countdown label="Next round in" target={status.next_round_at} serverClockOffsetMs={serverClockOffsetMs} />
        )}

        {status.countdown_target && (
          <Countdown label="Time remaining" target={status.countdown_target} serverClockOffsetMs={serverClockOffsetMs} />
        )}

        <Stack gap="xs">
          <Title order={2} size="h3">
            Services
          </Title>
          {status.services.length === 0 ? (
            <Text c="dimmed">No services configured.</Text>
          ) : (
            status.services.map((service) => (
              <ServiceRow key={service.name} service={service} onSelect={() => setHistoryOf(service.name)} />
            ))
          )}
        </Stack>
      </Stack>

      {historyOf !== null && <ServiceHistory key={historyOf} name={historyOf} onClose={() => setHistoryOf(null)} />}
    </Container>
  );
}

/** Which round this is and when the engine produced it: the sign the page is live. */
function Freshness({ status }: { status: Status }) {
  const engineBadge =
    status.engine_state === 'stopped' ? (
      <Badge color="gray">Engine stopped</Badge>
    ) : status.scoring_paused ? (
      <Badge color="yellow">Scoring paused</Badge>
    ) : null;

  return (
    <Group gap="sm">
      {engineBadge}
      <Text c="dimmed" size="sm">
        Round {status.round} · updated {new Date(status.server_time).toLocaleTimeString()}
      </Text>
    </Group>
  );
}

function TeamScore({ team, color, score }: { team: string; color: string; score: number }) {
  return (
    <Paper withBorder p="lg" radius="md">
      <Stack gap={0} align="center">
        <Text c={color} fw={700} tt="uppercase">
          {team}
        </Text>
        <Text fz={56} fw={800} lh={1.1}>
          {score}
        </Text>
      </Stack>
    </Paper>
  );
}

/** A service's name and up/down state; clicking it opens its history. */
function ServiceRow({ service, onSelect }: { service: ServiceStatus; onSelect: () => void }) {
  return (
    <UnstyledButton onClick={onSelect} aria-haspopup="dialog">
      <Paper withBorder px="md" py="sm" radius="md">
        <Group justify="space-between">
          <Text fw={500}>{service.name}</Text>
          <Badge color={service.up ? 'green' : 'red'} variant="filled">
            {service.up ? 'Up' : 'Down'}
          </Badge>
        </Group>
      </Paper>
    </UnstyledButton>
  );
}

/**
 * Time left until `target`, ticking every second between polls.
 *
 * "Now" is the engine's clock (this browser's clock plus the offset measured
 * from server_time), so every viewer sees the same remaining time however
 * far off their own clock is — the target was set against the engine's.
 */
function Countdown({
  label,
  target,
  serverClockOffsetMs,
}: {
  label: string;
  target: string;
  serverClockOffsetMs: number;
}) {
  const targetMs = Date.parse(target);
  const [serverNowMs, setServerNowMs] = useState(() => Date.now() + serverClockOffsetMs);

  useEffect(() => {
    const tick = () => setServerNowMs(Date.now() + serverClockOffsetMs);
    tick();
    const interval = setInterval(tick, 1_000);
    return () => clearInterval(interval);
  }, [serverClockOffsetMs]);

  if (Number.isNaN(targetMs)) {
    return null;
  }
  const remainingMs = Math.max(targetMs - serverNowMs, 0);

  return (
    <Paper withBorder p="md" radius="md">
      <Stack gap={0} align="center">
        <Text c="dimmed" size="sm" tt="uppercase">
          {label}
        </Text>
        <Text fz={40} fw={700} ff="monospace">
          {formatDuration(remainingMs)}
        </Text>
      </Stack>
    </Paper>
  );
}

/** Formats a duration as H:MM:SS, rounding up so it reads 0:00:00 only once time is up. */
function formatDuration(ms: number): string {
  const totalSeconds = Math.ceil(ms / 1_000);
  const hours = Math.floor(totalSeconds / 3_600);
  const minutes = Math.floor((totalSeconds % 3_600) / 60);
  const seconds = totalSeconds % 60;
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${hours}:${pad(minutes)}:${pad(seconds)}`;
}
