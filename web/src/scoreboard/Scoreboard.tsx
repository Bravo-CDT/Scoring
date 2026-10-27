import { Alert, Badge, Anchor, Center, Group, Loader, Stack, Text, Title, UnstyledButton } from '@mantine/core';
import { useEffect, useState } from 'react';
import type { Status } from '../api/client';
import { NetworkDiagram } from './NetworkDiagram';
import { ScoreBreakdown } from './ScoreBreakdown';
import classes from './Scoreboard.module.css';
import { ServiceHistory } from './ServiceHistory';
import { useStatusPolling } from './useStatusPolling';

/** The public scoreboard: scores, the clocks, and the network diagram of every service's state. */
export function Scoreboard() {
  const { status, error, serverClockOffsetMs } = useStatusPolling();
  // The service whose history is open, if any.
  const [historyOf, setHistoryOf] = useState<string | null>(null);
  // The team whose score breakdown is open, if any.
  const [breakdownOf, setBreakdownOf] = useState<'blue' | 'red' | null>(null);

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
    <div className={classes.page}>
      <header className={classes.header}>
        <Stack gap={4}>
          <Title order={1} className={classes.title}>
            {status.session_name}
          </Title>
          <Freshness status={status} />
        </Stack>

        <div className={classes.scores}>
          <TeamScore team="Blue" color="blue" score={status.blue_score} onOpen={() => setBreakdownOf('blue')} />
          <TeamScore team="Red" color="red" score={status.red_score} onOpen={() => setBreakdownOf('red')} />
        </div>

        <div className={classes.clocks}>
          {status.engine_state === 'running' && (
            <Countdown label="Next round in" target={status.next_round_at} serverClockOffsetMs={serverClockOffsetMs} />
          )}
          {status.countdown_target && (
            <Countdown label="Time remaining" target={status.countdown_target} serverClockOffsetMs={serverClockOffsetMs} />
          )}
        </div>
      </header>

      {error && (
        <Alert color="yellow" title="Connection lost" mb="lg">
          Showing the last update received; retrying. ({error.message})
        </Alert>
      )}

      <NetworkDiagram status={status} onSelectService={setHistoryOf} />

      <Group gap="md" className={classes.links}>
        {status.credentials_enabled && (
          <Anchor href="/credentials" c="dimmed" fz="xs">
            Credentials
          </Anchor>
        )}
        {status.injects_enabled && (
          <Anchor href="/injects" c="dimmed" fz="xs">
            Injects
          </Anchor>
        )}
        {status.injects_enabled && (
          <Anchor href="/hunt" c="dimmed" fz="xs">
            Scavenger hunt
          </Anchor>
        )}
        <Anchor href="/admin" c="dimmed" fz="xs">
          Admin
        </Anchor>
      </Group>

      {historyOf !== null && <ServiceHistory key={historyOf} name={historyOf} onClose={() => setHistoryOf(null)} />}
      {breakdownOf !== null && (
        <ScoreBreakdown
          key={breakdownOf}
          team={breakdownOf}
          label={breakdownOf === 'blue' ? 'Blue' : 'Red'}
          onClose={() => setBreakdownOf(null)}
        />
      )}
    </div>
  );
}

/** Which round this is and when the engine produced it: the sign the page is live. */
function Freshness({ status }: { status: Status }) {
  const engineBadge =
    status.engine_state === 'stopped' ? (
      <Badge color="gray">Engine stopped</Badge>
    ) : status.scoring_paused ? (
      <Badge color="yellow">Scoring paused</Badge>
    ) : (
      <Badge color="green">Live</Badge>
    );

  return (
    <Group gap="sm">
      {engineBadge}
      <Text c="dimmed" size="sm">
        Round {status.round} · updated {new Date(status.server_time).toLocaleTimeString()}
      </Text>
    </Group>
  );
}

/** A team's score; clicking it opens the breakdown of where the points came from. */
function TeamScore({ team, color, score, onOpen }: { team: string; color: string; score: number; onOpen: () => void }) {
  return (
    <UnstyledButton className={classes.team} data-team={color} onClick={onOpen}>
      <Text c={color} fw={800} tt="uppercase" fz="sm" lts={2}>
        {team}
      </Text>
      <Text className={classes.score}>{score.toLocaleString()}</Text>
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
    <div className={classes.clock}>
      <Text c="dimmed" fz="xs" tt="uppercase" lts={1}>
        {label}
      </Text>
      <Text className={classes.clockTime}>{formatDuration(remainingMs)}</Text>
    </div>
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
