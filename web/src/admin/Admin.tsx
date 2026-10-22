import {
  Alert,
  Anchor,
  Badge,
  Button,
  type ButtonProps,
  Center,
  Container,
  Group,
  List,
  Loader,
  Modal,
  NumberInput,
  Paper,
  Stack,
  Table,
  Text,
  Textarea,
  TextInput,
  Title,
} from '@mantine/core';
import { type FormEvent, type ReactNode, useEffect, useState } from 'react';
import {
  adjustScore,
  type AuthedFetch,
  describeError,
  getConfig,
  getConfigStatus,
  getStatus,
  pauseScoring,
  pauseServiceScoring,
  type ReloadResponse,
  reload,
  restartService,
  resumeScoring,
  resumeServiceScoring,
  saveConfig,
  type ServiceStatus,
  setCountdown,
  type StartFailure,
  type StartResponse,
  startEngine,
  startService,
  type Status,
  stopEngine,
  stopService,
} from '../api/client';
import { useStatusPolling } from '../scoreboard/useStatusPolling';
import { Login } from './Login';
import { useAdminSession } from './useAdminSession';

/**
 * The admin view at /admin: the login form until logged in, then the
 * console. A 401 from any admin action ends the session (see
 * useAdminSession), which brings the login form back.
 */
export function Admin() {
  const session = useAdminSession();
  if (session.token === null) {
    return <Login onLogin={session.login} sessionExpired={session.sessionExpired} />;
  }
  return <Console send={session.authedFetch} onLogout={session.logout} />;
}

/** What the latest admin action did, shown until the next one. */
interface Outcome {
  tone: 'success' | 'warning' | 'error';
  title: string;
  details: string[];
}

const toneColor: Record<Outcome['tone'], string> = { success: 'green', warning: 'yellow', error: 'red' };

function describeFailure(failure: StartFailure): string {
  return `${failure.service}: ${failure.error}`;
}

function reloadOutcome(result: ReloadResponse): Outcome {
  const details = [
    ...(result.error ? [result.error] : []),
    ...(result.added.length > 0 ? [`Added: ${result.added.join(', ')}`] : []),
    ...(result.removed.length > 0 ? [`Removed: ${result.removed.join(', ')}`] : []),
    ...result.failed.map((failure) => `Failed to start ${describeFailure(failure)}`),
  ];
  if (result.error) {
    return { tone: 'error', title: 'Reload failed', details };
  }
  if (result.failed.length > 0) {
    return { tone: 'warning', title: 'Reloaded, but some routines failed to start', details };
  }
  return { tone: 'success', title: 'Reloaded', details: details.length > 0 ? details : ['No changes.'] };
}

/** Whichever of two statuses the engine produced later. */
function newer(a: Status | null, b: Status | null): Status | null {
  if (a === null || b === null) return a ?? b;
  return Date.parse(b.server_time) > Date.parse(a.server_time) ? b : a;
}

/**
 * The control surface: engine, scoring, per-service, reload, and countdown
 * controls over the live status.
 *
 * The status shown is the newer of the public poll's and the one the latest
 * action answered with, so an action's effect shows at once rather than at
 * the next poll.
 */
function Console({ send, onLogout }: { send: AuthedFetch; onLogout: () => void }) {
  const { status: polled, error: pollError } = useStatusPolling();
  const [fromAction, setFromAction] = useState<Status | null>(null);
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  // One action at a time: every control is disabled while one is in flight.
  const [busy, setBusy] = useState(false);
  // Whether the config file has changed since the engine last reloaded it.
  // Set optimistically the moment a save succeeds (see ConfigEditor), and
  // refreshed from the server independently (e.g. someone edited the file
  // by hand instead), so either source can raise it; only a successful
  // reload lowers it.
  const [configStale, setConfigStale] = useState(false);

  const status = newer(polled, fromAction);

  useEffect(() => {
    let cancelled = false;
    async function pollConfigStatus() {
      try {
        const { stale } = await getConfigStatus(send);
        if (!cancelled && stale) setConfigStale(true);
      } catch {
        // Best-effort indicator; a failed poll leaves the last known value.
      }
    }
    void pollConfigStatus();
    const interval = setInterval(pollConfigStatus, 5_000);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, [send]);

  async function perform(title: string, action: (send: AuthedFetch) => Promise<Status | StartResponse>) {
    setBusy(true);
    try {
      const result = await action(send);
      setFromAction(result);
      const failed = 'failed' in result ? result.failed : [];
      setOutcome(
        failed.length > 0
          ? { tone: 'warning', title: `${title}: some routines failed to start`, details: failed.map(describeFailure) }
          : { tone: 'success', title: `${title}: done`, details: [] },
      );
    } catch (err) {
      setOutcome({ tone: 'error', title: `${title} failed`, details: [describeError(err)] });
    } finally {
      setBusy(false);
    }
  }

  async function performReload() {
    setBusy(true);
    try {
      setOutcome(reloadOutcome(await reload(send)));
      setConfigStale(false);
    } catch (err) {
      setOutcome({ tone: 'error', title: 'Reload failed', details: [describeError(err)] });
      setBusy(false);
      return;
    }
    // The reload answer has no status; fetch one so added and removed
    // services show without waiting for the next poll.
    try {
      setFromAction(await getStatus());
    } catch {
      // The poll catches up, and reports the failure if it persists.
    } finally {
      setBusy(false);
    }
  }

  return (
    <Container size="lg" py="xl">
      <Stack gap="lg">
        <Group justify="space-between" align="baseline">
          <Title order={1}>Admin{status ? ` · ${status.session_name}` : ''}</Title>
          <Group gap="sm">
            <Anchor href="/">Scoreboard</Anchor>
            <Button variant="default" onClick={onLogout}>
              Log out
            </Button>
          </Group>
        </Group>

        {outcome && (
          <Alert
            color={toneColor[outcome.tone]}
            title={outcome.title}
            withCloseButton
            closeButtonLabel="Dismiss"
            onClose={() => setOutcome(null)}
          >
            {outcome.details.length > 0 && (
              <List size="sm">
                {outcome.details.map((detail, i) => (
                  <List.Item key={i}>{detail}</List.Item>
                ))}
              </List>
            )}
          </Alert>
        )}

        {pollError && (
          <Alert color="yellow" title="Cannot refresh the status">
            Showing the last status received; retrying. ({pollError.message})
          </Alert>
        )}

        {status === null ? (
          <Center py="xl">
            <Loader />
          </Center>
        ) : (
          <>
            <EngineControls
              status={status}
              busy={busy}
              perform={perform}
              onReload={performReload}
              send={send}
              configStale={configStale}
              onConfigSaved={() => setConfigStale(true)}
            />
            <ScoreAdjustment status={status} busy={busy} perform={perform} />
            <CountdownControl status={status} busy={busy} perform={perform} />
            <ServiceControls services={status.services} busy={busy} perform={perform} />
          </>
        )}
      </Stack>
    </Container>
  );
}

type Perform = (title: string, action: (send: AuthedFetch) => Promise<Status | StartResponse>) => Promise<void>;

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Paper withBorder p="md" radius="md">
      <Stack gap="sm">
        <Title order={2} size="h4">
          {title}
        </Title>
        {children}
      </Stack>
    </Paper>
  );
}

function EngineControls({
  status,
  busy,
  perform,
  onReload,
  send,
  configStale,
  onConfigSaved,
}: {
  status: Status;
  busy: boolean;
  perform: Perform;
  onReload: () => void;
  send: AuthedFetch;
  configStale: boolean;
  onConfigSaved: () => void;
}) {
  const running = status.engine_state === 'running';
  return (
    <Section title="Engine">
      <Group gap="sm">
        <Badge color={running ? 'green' : 'gray'}>{running ? 'Running' : 'Stopped'}</Badge>
        <Badge color={status.scoring_paused ? 'yellow' : 'green'}>
          {status.scoring_paused ? 'Scoring paused' : 'Scoring'}
        </Badge>
        <Text c="dimmed" size="sm">
          Round {status.round} · Blue {status.blue_score} · Red {status.red_score}
        </Text>
      </Group>
      <Group gap="sm">
        {running ? (
          <ConfirmButton
            color="red"
            disabled={busy}
            confirmTitle="Stop the engine?"
            confirmMessage="Every check stops, and no service is scored until the engine is started again."
            confirmLabel="Stop engine"
            onConfirm={() => perform('Stop engine', stopEngine)}
          >
            Stop engine
          </ConfirmButton>
        ) : (
          <Button disabled={busy} onClick={() => perform('Start engine', startEngine)}>
            Start engine
          </Button>
        )}
        {status.scoring_paused ? (
          <Button variant="light" disabled={busy} onClick={() => perform('Resume scoring', resumeScoring)}>
            Resume scoring
          </Button>
        ) : (
          <Button variant="light" color="yellow" disabled={busy} onClick={() => perform('Pause scoring', pauseScoring)}>
            Pause scoring
          </Button>
        )}
        <ConfigEditor send={send} busy={busy} onSaved={onConfigSaved} />
        <Button variant="default" disabled={busy} onClick={onReload}>
          Reload config
        </Button>
        {configStale && <Badge color="yellow">New config detected</Badge>}
      </Group>
    </Section>
  );
}

/**
 * "View config": opens the raw config.yaml for editing and saves it back.
 * Save carries the hash the editor was opened with, so a stale save (the
 * file changed since) is rejected rather than silently overwritten; see
 * POST /config. Saving does not reload -- that is still the separate
 * "Reload config" button.
 */
function ConfigEditor({ send, busy, onSaved }: { send: AuthedFetch; busy: boolean; onSaved: () => void }) {
  const [opened, setOpened] = useState(false);
  const [text, setText] = useState('');
  const [hash, setHash] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);

  async function open() {
    setOpened(true);
    setError(null);
    setLoading(true);
    try {
      const cfg = await getConfig(send);
      setText(cfg.text);
      setHash(cfg.hash);
    } catch (err) {
      setError(describeError(err));
    } finally {
      setLoading(false);
    }
  }

  async function save() {
    setSaving(true);
    setError(null);
    try {
      await saveConfig(send, text, hash);
      setOpened(false);
      onSaved();
    } catch (err) {
      setError(describeError(err));
    } finally {
      setSaving(false);
    }
  }

  return (
    <>
      <Button variant="default" disabled={busy} onClick={open}>
        View config
      </Button>
      <Modal opened={opened} onClose={() => setOpened(false)} title="Edit config.yaml" size="xl">
        <Stack>
          {error && <Alert color="red">{error}</Alert>}
          <Textarea
            label="Config (raw YAML)"
            rows={20}
            value={text}
            onChange={(e) => setText(e.currentTarget.value)}
            disabled={loading}
            styles={{ input: { fontFamily: 'monospace' } }}
          />
          <Group justify="flex-end" gap="sm">
            <Button variant="default" onClick={() => setOpened(false)} disabled={saving}>
              Cancel
            </Button>
            <Button onClick={save} disabled={loading || saving}>
              Save
            </Button>
          </Group>
        </Stack>
      </Modal>
    </>
  );
}

/**
 * A manual, independent score correction: one shared points/reason pair,
 * applied to whichever team's button is clicked. Enabled only while scoring
 * is paused -- enforced server-side too, not just here.
 */
function ScoreAdjustment({ status, busy, perform }: { status: Status; busy: boolean; perform: Perform }) {
  const [points, setPoints] = useState<number | ''>('');
  const [reason, setReason] = useState('');
  const disabled = busy || !status.scoring_paused || points === '' || points === 0 || reason.trim() === '';

  function submit(side: 'blue' | 'red') {
    if (points === '') return;
    void perform(`Adjust ${side}`, (send) => adjustScore(send, side, points, reason));
  }

  return (
    <Section title="Scores">
      {!status.scoring_paused && (
        <Text c="dimmed" size="sm">
          Manual adjustments are only allowed while scoring is paused.
        </Text>
      )}
      <Group gap="sm" align="flex-end">
        <NumberInput
          label="Points"
          description="Positive adds, negative deducts"
          value={points}
          onChange={(v) => setPoints(typeof v === 'number' ? v : '')}
          disabled={!status.scoring_paused || busy}
        />
        <TextInput
          label="Reason"
          value={reason}
          onChange={(e) => setReason(e.currentTarget.value)}
          disabled={!status.scoring_paused || busy}
        />
        <Button disabled={disabled} onClick={() => submit('blue')}>
          Adjust blue
        </Button>
        <Button disabled={disabled} onClick={() => submit('red')}>
          Adjust red
        </Button>
      </Group>
    </Section>
  );
}

/**
 * Sets or clears the scoreboard's countdown. The input is in this browser's
 * local time; it is sent as an absolute instant, so the engine's time zone
 * does not matter.
 */
function CountdownControl({ status, busy, perform }: { status: Status; busy: boolean; perform: Perform }) {
  const [value, setValue] = useState('');
  const [invalid, setInvalid] = useState(false);

  function submit(event: FormEvent) {
    event.preventDefault();
    // A datetime-local value has no offset, so Date reads it as local time.
    const target = new Date(value);
    if (Number.isNaN(target.getTime())) {
      setInvalid(true);
      return;
    }
    setInvalid(false);
    void perform('Set countdown', (send) => setCountdown(send, target.toISOString()));
  }

  return (
    <Section title="Countdown">
      <Text size="sm">
        {status.countdown_target
          ? `Counting down to ${new Date(status.countdown_target).toLocaleString()}.`
          : 'No countdown is set.'}
      </Text>
      <form onSubmit={submit}>
        <Group gap="sm" align="flex-end">
          <TextInput
            type="datetime-local"
            label="Count down to (your local time)"
            required
            value={value}
            error={invalid ? 'Enter a date and time' : undefined}
            onChange={(e) => setValue(e.currentTarget.value)}
          />
          <Button type="submit" disabled={busy}>
            Set countdown
          </Button>
          <Button
            variant="default"
            disabled={busy || !status.countdown_target}
            onClick={() => perform('Clear countdown', (send) => setCountdown(send, null))}
          >
            Clear countdown
          </Button>
        </Group>
      </form>
    </Section>
  );
}

function ServiceControls({ services, busy, perform }: { services: ServiceStatus[]; busy: boolean; perform: Perform }) {
  return (
    <Section title="Services">
      {services.length === 0 ? (
        <Text c="dimmed">No services configured.</Text>
      ) : (
        <Table.ScrollContainer minWidth={640}>
          <Table verticalSpacing="sm">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Service</Table.Th>
                <Table.Th>State</Table.Th>
                <Table.Th>Actions</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {services.map((service) => (
                <ServiceRow key={service.name} service={service} busy={busy} perform={perform} />
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
    </Section>
  );
}

function ServiceRow({ service, busy, perform }: { service: ServiceStatus; busy: boolean; perform: Perform }) {
  const { name } = service;
  return (
    <Table.Tr>
      <Table.Td fw={500}>{name}</Table.Td>
      <Table.Td>
        <Group gap={6}>
          {service.checking && (
            <Badge color="blue" variant="light">
              Scoring…
            </Badge>
          )}
          <Badge color={service.up ? 'green' : 'red'} variant="filled">
            {service.up ? 'Up' : 'Down'}
          </Badge>
          <Badge color={service.routine_enabled ? 'green' : 'gray'}>
            {service.routine_enabled ? 'Checking' : 'Stopped'}
          </Badge>
          {service.scoring_paused && <Badge color="yellow">Scoring paused</Badge>}
        </Group>
      </Table.Td>
      <Table.Td>
        <Group gap="xs" wrap="nowrap">
          {service.routine_enabled ? (
            <ConfirmButton
              size="xs"
              color="red"
              variant="light"
              disabled={busy}
              aria-label={`Stop ${name}`}
              confirmTitle={`Stop ${name}?`}
              confirmMessage={`${name} stops being checked and scored until it is started again.`}
              confirmLabel={`Stop ${name}`}
              onConfirm={() => perform(`Stop ${name}`, (send) => stopService(send, name))}
            >
              Stop
            </ConfirmButton>
          ) : (
            <Button
              size="xs"
              variant="light"
              disabled={busy}
              aria-label={`Start ${name}`}
              onClick={() => perform(`Start ${name}`, (send) => startService(send, name))}
            >
              Start
            </Button>
          )}
          <Button
            size="xs"
            variant="default"
            disabled={busy}
            aria-label={`Restart ${name}`}
            onClick={() => perform(`Restart ${name}`, (send) => restartService(send, name))}
          >
            Restart
          </Button>
          {service.scoring_paused ? (
            <Button
              size="xs"
              variant="default"
              disabled={busy}
              aria-label={`Resume scoring ${name}`}
              onClick={() => perform(`Resume scoring ${name}`, (send) => resumeServiceScoring(send, name))}
            >
              Resume scoring
            </Button>
          ) : (
            <Button
              size="xs"
              variant="default"
              disabled={busy}
              aria-label={`Pause scoring ${name}`}
              onClick={() => perform(`Pause scoring ${name}`, (send) => pauseServiceScoring(send, name))}
            >
              Pause scoring
            </Button>
          )}
        </Group>
      </Table.Td>
    </Table.Tr>
  );
}

interface ConfirmButtonProps extends ButtonProps {
  disabled?: boolean;
  'aria-label'?: string;
  confirmTitle: string;
  confirmMessage: string;
  confirmLabel: string;
  onConfirm: () => void;
  children: ReactNode;
}

/**
 * A button for a destructive action: clicking it asks for confirmation, and
 * only confirming runs the action. Cancel has the initial focus, so a stray
 * Enter does not confirm.
 */
function ConfirmButton({
  confirmTitle,
  confirmMessage,
  confirmLabel,
  onConfirm,
  children,
  ...buttonProps
}: ConfirmButtonProps) {
  const [opened, setOpened] = useState(false);
  const close = () => setOpened(false);
  return (
    <>
      <Button {...buttonProps} onClick={() => setOpened(true)} aria-haspopup="dialog">
        {children}
      </Button>
      <Modal opened={opened} onClose={close} title={confirmTitle} closeButtonProps={{ 'aria-label': 'Close' }}>
        <Stack>
          <Text size="sm">{confirmMessage}</Text>
          <Group justify="flex-end" gap="sm">
            <Button variant="default" onClick={close} data-autofocus>
              Cancel
            </Button>
            <Button
              color="red"
              onClick={() => {
                close();
                onConfirm();
              }}
            >
              {confirmLabel}
            </Button>
          </Group>
        </Stack>
      </Modal>
    </>
  );
}
