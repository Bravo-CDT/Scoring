import { Alert, Anchor, Badge, Button, Container, Group, Loader, NativeSelect, Paper, Stack, Text, TextInput, Title } from '@mantine/core';
import { type FormEvent, useCallback, useEffect, useState } from 'react';
import { type AuthedFetch, claimHunt, describeError, getHunts, type HuntView } from '../api/client';
import { Login } from '../admin/Login';
import { useTeamSession } from '../admin/useAdminSession';

/** How often the list is refetched, to pick up hunts being opened, closed and claimed. */
const POLL_MS = 15_000;

/**
 * A team's scavenger hunt page: a form to claim a hunt with its code, and
 * every hunt it can see with who, if anyone, has claimed it.
 */
export function HuntPage() {
  const session = useTeamSession();
  if (session.token === null) {
    return <Login onLogin={session.login} sessionExpired={session.sessionExpired} title="Team login" />;
  }
  return <HuntList send={session.authedFetch} onLogout={session.logout} />;
}

function HuntList({ send, onLogout }: { send: AuthedFetch; onLogout: () => void }) {
  const [hunts, setHunts] = useState<HuntView[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    const load = () =>
      getHunts(send).then(
        (list) => {
          if (cancelled) return;
          setHunts(list);
          setError(null);
        },
        (err: unknown) => !cancelled && setError(describeError(err)),
      );
    void load();
    const timer = setInterval(load, POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [send]);

  const replace = useCallback((hunt: HuntView) => {
    setHunts((list) => list && list.map((h) => (h.id === hunt.id ? hunt : h)));
  }, []);

  return (
    <Container size="sm" py="xl">
      <Stack gap="lg">
        <Group justify="space-between" align="baseline">
          <Title order={1}>Scavenger hunt</Title>
          <Group gap="md">
            <Anchor href="/" fz="sm">
              Scoreboard
            </Anchor>
            <Button variant="subtle" size="xs" onClick={onLogout}>
              Log out
            </Button>
          </Group>
        </Group>
        {error && <Alert color="red">{error}</Alert>}
        {hunts === null && !error && <Loader />}
        {hunts && <ClaimForm hunts={hunts} send={send} onClaimed={replace} />}
        {hunts?.length === 0 && <Text c="dimmed">No hunts yet.</Text>}
        {hunts?.map((hunt) => (
          <HuntRow key={hunt.id} hunt={hunt} />
        ))}
      </Stack>
    </Container>
  );
}

function ClaimForm({
  hunts,
  send,
  onClaimed,
}: {
  hunts: HuntView[];
  send: AuthedFetch;
  onClaimed: (hunt: HuntView) => void;
}) {
  const [selected, setSelected] = useState('');
  const [code, setCode] = useState('');
  const [saving, setSaving] = useState(false);
  const [outcome, setOutcome] = useState<{ ok: boolean; message: string } | null>(null);
  const claimable = hunts.filter((h) => h.claimable);
  // A poll can drop the picked hunt (claimed by the other team, closed); then nothing is picked.
  const value = claimable.some((h) => String(h.id) === selected) ? selected : '';

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!value || code === '') return;
    setSaving(true);
    setOutcome(null);
    try {
      onClaimed(await claimHunt(send, Number(value), code));
      setSelected('');
      setCode('');
      setOutcome({ ok: true, message: 'Claimed' });
    } catch (err) {
      setOutcome({ ok: false, message: describeError(err) });
    } finally {
      setSaving(false);
    }
  }

  return (
    <form onSubmit={submit}>
      <Stack gap="xs">
        <NativeSelect
          label="Hunt"
          value={value}
          onChange={(e) => setSelected(e.currentTarget.value)}
          data={[{ value: '', label: 'Pick a hunt' }, ...claimable.map((h) => ({ value: String(h.id), label: h.title }))]}
        />
        <TextInput label="Code" value={code} onChange={(e) => setCode(e.currentTarget.value)} />
        <Group>
          <Button type="submit" loading={saving} disabled={!value || code === ''}>
            Submit
          </Button>
        </Group>
        {outcome && (
          <Text c={outcome.ok ? 'green' : 'red'} fz="sm">
            {outcome.message}
          </Text>
        )}
      </Stack>
    </form>
  );
}

function stateOf(hunt: HuntView): { label: string; color: string } {
  if (hunt.claimed_by) return { label: `Claimed by ${hunt.claimed_by}`, color: 'green' };
  if (hunt.claimable) return { label: 'Open', color: 'cyan' };
  return { label: 'Closed', color: 'gray' };
}

function HuntRow({ hunt }: { hunt: HuntView }) {
  const state = stateOf(hunt);
  return (
    <Paper withBorder p="md" role="group" aria-label={hunt.title}>
      <Group justify="space-between" mb="xs">
        <Text fw={700} fz="lg">
          {hunt.title}
        </Text>
        <Badge color={state.color} variant="light">
          {state.label}
        </Badge>
      </Group>
      <Stack gap="xs">
        <Text fz="sm">{hunt.points.toLocaleString()} points</Text>
        <Text fz="sm" c="dimmed">
          Due {new Date(hunt.deadline).toLocaleString()}
        </Text>
        {hunt.description !== '' && <Text style={{ whiteSpace: 'pre-wrap' }}>{hunt.description}</Text>}
      </Stack>
    </Paper>
  );
}
