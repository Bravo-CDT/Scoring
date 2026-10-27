import { Alert, Badge, Button, Group, NativeSelect, NumberInput, Paper, Stack, Text, TextInput, Title } from '@mantine/core';
import { type FormEvent, useCallback, useEffect, useState } from 'react';
import {
  approveInject,
  type AuthedFetch,
  createInject,
  deleteInject,
  describeError,
  downloadFile,
  getInjects,
  gradeInject,
  type InjectView,
  updateInject,
  uploadInjectPdf,
} from '../api/client';
import { saveBlob } from '../injects/download';
import { ConfirmButton } from './ConfirmButton';
import { fromLocalInput, toLocalInput } from './localTime';

/** How often the list is refetched, to show submissions as they come in. */
const refreshMs = 10_000;

/** The points approving inject at its grade moves. */
function pointsFor(inject: InjectView): number {
  return Math.trunc(((inject.grade ?? 0) * inject.worth) / 10);
}

/**
 * Every inject, for the grey team: create them, upload their instructions,
 * open and close them, grade submissions and approve, which moves the points.
 */
export function InjectsSection({ send }: { send: AuthedFetch }) {
  const [injects, setInjects] = useState<InjectView[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  // Kept apart from error, which each successful poll clears.
  const [actionError, setActionError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    async function load() {
      try {
        const loaded = await getInjects(send);
        if (!cancelled) {
          setInjects(loaded);
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

  const replace = useCallback((inject: InjectView) => {
    setInjects((list) => {
      const current = list ?? [];
      return current.some((i) => i.id === inject.id)
        ? current.map((i) => (i.id === inject.id ? inject : i))
        : [...current, inject];
    });
  }, []);

  const remove = useCallback((id: number) => {
    setInjects((list) => list && list.filter((i) => i.id !== id));
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
          Injects
        </Title>
        {error && <Alert color="red">{error}</Alert>}
        {actionError && <Alert color="red">{actionError}</Alert>}
        <CreateInjectForm send={send} run={run} onCreated={replace} />
        {injects?.length === 0 && <Text c="dimmed">No injects yet.</Text>}
        {injects?.map((inject) => (
          <InjectCard key={inject.id} inject={inject} send={send} run={run} onChange={replace} onDelete={remove} />
        ))}
      </Stack>
    </Paper>
  );
}

type Run = (what: string, action: () => Promise<void>) => Promise<void>;

function CreateInjectForm({
  send,
  run,
  onCreated,
}: {
  send: AuthedFetch;
  run: Run;
  onCreated: (inject: InjectView) => void;
}) {
  const [title, setTitle] = useState('');
  const [team, setTeam] = useState<'blue' | 'red'>('blue');
  const [deadline, setDeadline] = useState('');
  const [saving, setSaving] = useState(false);

  async function submit(event: FormEvent) {
    event.preventDefault();
    const iso = fromLocalInput(deadline);
    if (!iso) return;
    setSaving(true);
    await run('Create inject', async () => {
      onCreated(await createInject(send, { title, team, deadline: iso }));
      setTitle('');
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
          data={['blue', 'red']}
          value={team}
          onChange={(e) => setTeam(e.currentTarget.value as 'blue' | 'red')}
        />
        <TextInput
          label="Deadline"
          type="datetime-local"
          required
          value={deadline}
          onChange={(e) => setDeadline(e.currentTarget.value)}
        />
        <Button type="submit" loading={saving}>
          Create
        </Button>
      </Group>
    </form>
  );
}

function stateOf(inject: InjectView): { label: string; color: string } {
  if (inject.approved_at) return { label: 'Approved', color: 'green' };
  if (inject.accepting) return { label: 'Open', color: 'cyan' };
  if (inject.open) return { label: 'Past deadline', color: 'orange' };
  return { label: 'Closed', color: 'gray' };
}

function InjectCard({
  inject,
  send,
  run,
  onChange,
  onDelete,
}: {
  inject: InjectView;
  send: AuthedFetch;
  run: Run;
  onChange: (inject: InjectView) => void;
  onDelete: (id: number) => void;
}) {
  const [deadline, setDeadline] = useState(() => toLocalInput(inject.deadline));
  const [grade, setGrade] = useState<number | string>(inject.grade ?? '');
  const [busy, setBusy] = useState(false);
  const state = stateOf(inject);
  const approved = inject.approved_at !== undefined;
  const pdfId = `inject-${inject.id}-pdf`;

  // Follow saved changes (this browser's or another admin's, via the poll).
  const [shown, setShown] = useState({ deadline: inject.deadline, grade: inject.grade });
  if (shown.deadline !== inject.deadline || shown.grade !== inject.grade) {
    setShown({ deadline: inject.deadline, grade: inject.grade });
    setDeadline(toLocalInput(inject.deadline));
    setGrade(inject.grade ?? '');
  }

  async function act(what: string, action: () => Promise<void>) {
    setBusy(true);
    await run(what, action);
    setBusy(false);
  }

  const update = (open: boolean) =>
    act('Update inject', async () => {
      const iso = fromLocalInput(deadline);
      if (!iso) throw new Error('the deadline is not a valid time');
      onChange(await updateInject(send, inject.id, { title: inject.title, deadline: iso, open }));
    });

  const points = pointsFor(inject);
  const to = inject.team === 'blue' ? 'to blue' : 'from blue to red';

  return (
    <Paper withBorder p="sm" role="group" aria-label={inject.title}>
      <Stack gap="xs">
        <Group justify="space-between">
          <Group gap="xs">
            <Text fw={700}>{inject.title}</Text>
            <Badge color={inject.team} variant="light">
              {inject.team}
            </Badge>
          </Group>
          <Badge color={state.color} variant="light">
            {state.label}
          </Badge>
        </Group>

        <Group align="flex-end" gap="sm" wrap="wrap">
          <TextInput
            label="Deadline"
            type="datetime-local"
            size="xs"
            disabled={approved}
            value={deadline}
            onChange={(e) => setDeadline(e.currentTarget.value)}
          />
          <Button size="xs" variant="default" disabled={approved || busy} onClick={() => void update(inject.open)}>
            Save deadline
          </Button>
          <Button size="xs" variant="light" disabled={approved || busy} onClick={() => void update(!inject.open)}>
            {inject.open ? 'Close' : 'Open'}
          </Button>
        </Group>

        <Group align="center" gap="sm">
          <Text component="label" htmlFor={pdfId} fz="sm">
            Instructions PDF{inject.has_pdf ? ' (uploaded)' : ''}
          </Text>
          <input
            id={pdfId}
            type="file"
            accept="application/pdf,.pdf"
            disabled={busy}
            onChange={(e) => {
              const file = e.currentTarget.files?.[0];
              const input = e.currentTarget;
              if (file) {
                void act('Upload PDF', async () => {
                  onChange(await uploadInjectPdf(send, inject.id, file));
                  input.value = '';
                });
              }
            }}
          />
        </Group>

        {inject.submission ? (
          <Group gap="xs" align="baseline">
            <Text fz="sm">
              Submitted {new Date(inject.submission.submitted_at).toLocaleString()}: {inject.submission.file_name}
            </Text>
            <Button
              size="compact-xs"
              variant="subtle"
              onClick={() =>
                void run('Download', async () => {
                  const sub = inject.submission;
                  if (sub) saveBlob(await downloadFile(send, `/team/injects/${inject.id}/submission`), sub.file_name);
                })
              }
            >
              Download
            </Button>
            {inject.submission.note && (
              <Text fz="sm" c="dimmed" w="100%" style={{ whiteSpace: 'pre-wrap' }}>
                {inject.submission.note}
              </Text>
            )}
          </Group>
        ) : (
          <Text fz="sm" c="dimmed">
            No submission yet.
          </Text>
        )}

        {approved ? (
          <Text fw={600}>
            Approved: {inject.grade}/10, {(inject.points ?? 0).toLocaleString()} points {to}
          </Text>
        ) : (
          <Group align="flex-end" gap="sm" wrap="wrap">
            <NumberInput
              label="Grade (0-10)"
              size="xs"
              min={0}
              max={10}
              allowDecimal={false}
              w={110}
              disabled={!inject.submission}
              value={grade}
              onChange={setGrade}
            />
            <Button
              size="xs"
              variant="default"
              disabled={!inject.submission || grade === '' || busy}
              onClick={() =>
                void act('Save grade', async () => {
                  onChange(await gradeInject(send, inject.id, Number(grade)));
                })
              }
            >
              Save grade
            </Button>
            <ConfirmButton
              size="xs"
              disabled={inject.grade === undefined || inject.accepting || busy}
              confirmTitle={`Approve ${inject.title}?`}
              confirmMessage={`Graded ${inject.grade ?? 0}/10: moves ${points.toLocaleString()} points ${to}. This can't be undone, except by deleting the inject.`}
              confirmLabel="Approve"
              onConfirm={() =>
                void act('Approve', async () => {
                  onChange(await approveInject(send, inject.id));
                })
              }
            >
              Approve ({points.toLocaleString()})
            </ConfirmButton>
          </Group>
        )}

        <Group justify="flex-end">
          <ConfirmButton
            size="xs"
            color="red"
            variant="subtle"
            disabled={busy}
            confirmTitle={`Delete ${inject.title}?`}
            confirmMessage={
              approved
                ? `Deletes the inject and its files, and takes back the ${(inject.points ?? 0).toLocaleString()} points it moved.`
                : 'Deletes the inject and its files.'
            }
            confirmLabel="Delete"
            onConfirm={() =>
              void act('Delete', async () => {
                await deleteInject(send, inject.id);
                onDelete(inject.id);
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
