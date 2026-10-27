import {
  Alert,
  Badge,
  Button,
  Group,
  NativeSelect,
  NumberInput,
  Paper,
  Stack,
  Text,
  Textarea,
  TextInput,
  Title,
} from '@mantine/core';
import { type FormEvent, useCallback, useEffect, useState } from 'react';
import {
  type AuthedFetch,
  createHunt,
  deleteHunt,
  describeError,
  getHunts,
  type HuntView,
  updateHunt,
} from '../api/client';
import { ConfirmButton } from './ConfirmButton';
import { fromLocalInput, toLocalInput } from './localTime';

/** How often the list is refetched, to show claims as they come in. */
const refreshMs = 10_000;

/**
 * Every scavenger hunt, for the grey team: create them, edit the code, points
 * and deadline, open and close them, and delete them, which takes back any
 * points a claim moved.
 */
export function HuntsSection({ send }: { send: AuthedFetch }) {
  const [hunts, setHunts] = useState<HuntView[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  // Kept apart from error, which each successful poll clears.
  const [actionError, setActionError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    async function load() {
      try {
        const loaded = await getHunts(send);
        if (!cancelled) {
          setHunts(loaded);
          setError(null);
        }
      } catch (err) {
        if (!cancelled) setError(describeError(err));
      }
    }
    void load();
    const interval = setInterval(load, refreshMs);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, [send]);

  const replace = useCallback((hunt: HuntView) => {
    setHunts((list) => {
      const current = list ?? [];
      return current.some((h) => h.id === hunt.id)
        ? current.map((h) => (h.id === hunt.id ? hunt : h))
        : [...current, hunt];
    });
  }, []);

  const remove = useCallback((id: number) => {
    setHunts((list) => list && list.filter((h) => h.id !== id));
  }, []);

  /** Runs an action, reporting its failure as what. */
  const run = useCallback(async (what: string, action: () => Promise<void>) => {
    setActionError(null);
    try {
      await action();
    } catch (err) {
      setActionError(`${what} failed: ${describeError(err)}`);
    }
  }, []);

  return (
    <Paper withBorder p="md" radius="md">
      <Stack gap="sm">
        <Title order={2} size="h4">
          Scavenger hunt
        </Title>
        {error && <Alert color="red">{error}</Alert>}
        {actionError && <Alert color="red">{actionError}</Alert>}
        <CreateHuntForm send={send} run={run} onCreated={replace} />
        {hunts?.length === 0 && <Text c="dimmed">No hunts yet.</Text>}
        {hunts?.map((hunt) => (
          <HuntCard key={hunt.id} hunt={hunt} send={send} run={run} onChange={replace} onDelete={remove} />
        ))}
      </Stack>
    </Paper>
  );
}

type Run = (what: string, action: () => Promise<void>) => Promise<void>;

function CreateHuntForm({
  send,
  run,
  onCreated,
}: {
  send: AuthedFetch;
  run: Run;
  onCreated: (hunt: HuntView) => void;
}) {
  const [title, setTitle] = useState('');
  const [team, setTeam] = useState<'blue' | 'red' | 'either'>('blue');
  const [points, setPoints] = useState<number | string>(100);
  const [code, setCode] = useState('');
  const [description, setDescription] = useState('');
  const [deadline, setDeadline] = useState('');
  const [saving, setSaving] = useState(false);

  async function submit(event: FormEvent) {
    event.preventDefault();
    const iso = fromLocalInput(deadline);
    if (!iso || points === '') return;
    setSaving(true);
    await run('Create hunt', async () => {
      onCreated(await createHunt(send, { title, team, description, code, points: Number(points), deadline: iso }));
      setTitle('');
      setCode('');
      setDescription('');
      setDeadline('');
    });
    setSaving(false);
  }

  return (
    <form onSubmit={submit}>
      <Group align="flex-end" gap="sm" wrap="wrap">
        <TextInput label="Title" required value={title} onChange={(e) => setTitle(e.currentTarget.value)} />
        <NativeSelect
          label="Team"
          data={['blue', 'red', 'either']}
          value={team}
          onChange={(e) => setTeam(e.currentTarget.value as 'blue' | 'red' | 'either')}
        />
        <NumberInput label="Points" required min={1} allowDecimal={false} w={110} value={points} onChange={setPoints} />
        <TextInput label="Code" required value={code} onChange={(e) => setCode(e.currentTarget.value)} />
        <TextInput
          label="Deadline"
          type="datetime-local"
          required
          value={deadline}
          onChange={(e) => setDeadline(e.currentTarget.value)}
        />
        <Textarea
          label="Description"
          maxLength={2000}
          w="100%"
          value={description}
          onChange={(e) => setDescription(e.currentTarget.value)}
        />
        <Button type="submit" loading={saving}>
          Create
        </Button>
      </Group>
    </form>
  );
}

function stateOf(hunt: HuntView): { label: string; color: string } {
  if (hunt.claimed_by) return { label: `Claimed by ${hunt.claimed_by}`, color: 'green' };
  if (hunt.claimable) return { label: 'Open', color: 'cyan' };
  if (hunt.open) return { label: 'Past deadline', color: 'orange' };
  return { label: 'Closed', color: 'gray' };
}

function HuntCard({
  hunt,
  send,
  run,
  onChange,
  onDelete,
}: {
  hunt: HuntView;
  send: AuthedFetch;
  run: Run;
  onChange: (hunt: HuntView) => void;
  onDelete: (id: number) => void;
}) {
  const [title, setTitle] = useState(hunt.title);
  const [code, setCode] = useState(hunt.code ?? '');
  const [points, setPoints] = useState<number | string>(hunt.points);
  const [description, setDescription] = useState(hunt.description);
  const [deadline, setDeadline] = useState(() => toLocalInput(hunt.deadline));
  const [busy, setBusy] = useState(false);
  const state = stateOf(hunt);
  const locked = hunt.claimed_by !== undefined;

  // Follow saved changes (this browser's or another admin's, via the poll).
  const [shown, setShown] = useState({
    title: hunt.title,
    code: hunt.code,
    points: hunt.points,
    description: hunt.description,
    deadline: hunt.deadline,
  });
  if (
    shown.title !== hunt.title ||
    shown.code !== hunt.code ||
    shown.points !== hunt.points ||
    shown.description !== hunt.description ||
    shown.deadline !== hunt.deadline
  ) {
    setShown({
      title: hunt.title,
      code: hunt.code,
      points: hunt.points,
      description: hunt.description,
      deadline: hunt.deadline,
    });
    setTitle(hunt.title);
    setCode(hunt.code ?? '');
    setPoints(hunt.points);
    setDescription(hunt.description);
    setDeadline(toLocalInput(hunt.deadline));
  }

  async function act(what: string, action: () => Promise<void>) {
    setBusy(true);
    await run(what, action);
    setBusy(false);
  }

  const update = (open: boolean) =>
    act('Update hunt', async () => {
      const iso = fromLocalInput(deadline);
      if (!iso) throw new Error('the deadline is not a valid time');
      if (points === '') throw new Error('the points are not a number');
      onChange(await updateHunt(send, hunt.id, { title, code, description, points: Number(points), deadline: iso, open }));
    });

  return (
    <Paper withBorder p="sm" role="group" aria-label={hunt.title}>
      <Stack gap="xs">
        <Group justify="space-between">
          <Group gap="xs">
            <Text fw={700}>{hunt.title}</Text>
            <Badge color={hunt.team === 'either' ? 'grape' : hunt.team} variant="light">
              {hunt.team}
            </Badge>
          </Group>
          <Badge color={state.color} variant="light">
            {state.label}
          </Badge>
        </Group>

        <Group align="flex-end" gap="sm" wrap="wrap">
          <TextInput
            label="Title"
            size="xs"
            disabled={locked}
            value={title}
            onChange={(e) => setTitle(e.currentTarget.value)}
          />
          <TextInput
            label="Code"
            size="xs"
            disabled={locked}
            value={code}
            onChange={(e) => setCode(e.currentTarget.value)}
          />
          <NumberInput
            label="Points"
            size="xs"
            min={1}
            allowDecimal={false}
            w={110}
            disabled={locked}
            value={points}
            onChange={setPoints}
          />
          <TextInput
            label="Deadline"
            type="datetime-local"
            size="xs"
            disabled={locked}
            value={deadline}
            onChange={(e) => setDeadline(e.currentTarget.value)}
          />
          <Textarea
            label="Description"
            size="xs"
            maxLength={2000}
            w="100%"
            disabled={locked}
            value={description}
            onChange={(e) => setDescription(e.currentTarget.value)}
          />
        </Group>

        <Group gap="sm">
          <Button size="xs" variant="default" disabled={locked || busy} onClick={() => void update(hunt.open)}>
            Save
          </Button>
          <Button size="xs" variant="light" disabled={locked || busy} onClick={() => void update(!hunt.open)}>
            {hunt.open ? 'Close' : 'Open'}
          </Button>
        </Group>

        {locked && (
          <Text fw={600}>
            Claimed by {hunt.claimed_by}
            {hunt.claimed_at ? ` at ${new Date(hunt.claimed_at).toLocaleString()}` : ''}:{' '}
            {(hunt.points_moved ?? 0).toLocaleString()} points
          </Text>
        )}

        <Group justify="flex-end">
          <ConfirmButton
            size="xs"
            color="red"
            variant="subtle"
            disabled={busy}
            confirmTitle={`Delete ${hunt.title}?`}
            confirmMessage={
              locked
                ? `Deletes the hunt and takes back the ${(hunt.points_moved ?? 0).toLocaleString()} points its claim moved.`
                : 'Deletes the hunt.'
            }
            confirmLabel="Delete"
            onConfirm={() =>
              void act('Delete', async () => {
                await deleteHunt(send, hunt.id);
                onDelete(hunt.id);
              })
            }
          >
            Delete
          </ConfirmButton>
        </Group>
      </Stack>
    </Paper>
  );
}
