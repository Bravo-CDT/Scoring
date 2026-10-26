import { Alert, Anchor, Badge, Button, Container, Group, Loader, Paper, Stack, Text, Textarea, Title } from '@mantine/core';
import { type FormEvent, useCallback, useEffect, useState } from 'react';
import { type AuthedFetch, describeError, downloadFile, getInjects, type InjectView, submitInject } from '../api/client';
import { Login } from '../admin/Login';
import { useTeamSession } from '../admin/useAdminSession';
import { saveBlob } from './download';

/** How often the list is refetched, to pick up injects being opened and closed. */
const POLL_MS = 15_000;

/**
 * A team's injects page: each of its injects with the instructions to
 * download, a form to submit while it's open, and the grade once approved.
 */
export function InjectsPage() {
  const session = useTeamSession();
  if (session.token === null) {
    return <Login onLogin={session.login} sessionExpired={session.sessionExpired} title="Team login" />;
  }
  return <InjectsList send={session.authedFetch} onLogout={session.logout} />;
}

function InjectsList({ send, onLogout }: { send: AuthedFetch; onLogout: () => void }) {
  const [injects, setInjects] = useState<InjectView[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    const load = () =>
      getInjects(send).then(
        (list) => {
          if (cancelled) return;
          setInjects(list);
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

  const replace = useCallback((inject: InjectView) => {
    setInjects((list) => list && list.map((i) => (i.id === inject.id ? inject : i)));
  }, []);

  return (
    <Container size="sm" py="xl">
      <Stack gap="lg">
        <Group justify="space-between" align="baseline">
          <Title order={1}>Injects</Title>
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
        {injects === null && !error && <Loader />}
        {injects?.length === 0 && <Text c="dimmed">No injects yet.</Text>}
        {injects?.map((inject) => (
          <InjectRow key={inject.id} inject={inject} send={send} onChange={replace} />
        ))}
      </Stack>
    </Container>
  );
}

function stateOf(inject: InjectView): { label: string; color: string } {
  if (inject.approved_at) return { label: 'Approved', color: 'green' };
  if (inject.accepting) return { label: 'Open', color: 'cyan' };
  return { label: 'Closed', color: 'gray' };
}

function InjectRow({
  inject,
  send,
  onChange,
}: {
  inject: InjectView;
  send: AuthedFetch;
  onChange: (inject: InjectView) => void;
}) {
  const [note, setNote] = useState('');
  const [file, setFile] = useState<File | null>(null);
  const [saving, setSaving] = useState(false);
  const [outcome, setOutcome] = useState<{ ok: boolean; message: string } | null>(null);
  const state = stateOf(inject);
  const fileId = `inject-${inject.id}-file`;

  async function download(path: string, name: string) {
    try {
      saveBlob(await downloadFile(send, path), name);
    } catch (err) {
      setOutcome({ ok: false, message: describeError(err) });
    }
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!file) return;
    const form = event.currentTarget;
    setSaving(true);
    setOutcome(null);
    try {
      onChange(await submitInject(send, inject.id, note, file));
      setNote('');
      setFile(null);
      form.reset();
      setOutcome({ ok: true, message: 'Submitted' });
    } catch (err) {
      setOutcome({ ok: false, message: describeError(err) });
    } finally {
      setSaving(false);
    }
  }

  return (
    <Paper withBorder p="md" role="group" aria-label={inject.title}>
      <Group justify="space-between" mb="xs">
        <Text fw={700} fz="lg">
          {inject.title}
        </Text>
        <Badge color={state.color} variant="light">
          {state.label}
        </Badge>
      </Group>
      <Stack gap="xs">
        <Text fz="sm" c="dimmed">
          Due {new Date(inject.deadline).toLocaleString()}
        </Text>
        {inject.has_pdf && (
          <Group>
            <Button
              variant="light"
              size="xs"
              onClick={() => download(`/team/injects/${inject.id}/pdf`, `${inject.title}.pdf`)}
            >
              Download instructions
            </Button>
          </Group>
        )}
        {inject.submission && (
          <Group gap="xs">
            <Text fz="sm">
              Submitted {inject.submission.file_name} at {new Date(inject.submission.submitted_at).toLocaleString()}
            </Text>
            <Anchor
              component="button"
              type="button"
              fz="sm"
              onClick={() => {
                const sub = inject.submission;
                if (sub) void download(`/team/injects/${inject.id}/submission`, sub.file_name);
              }}
            >
              Download
            </Anchor>
          </Group>
        )}
        {inject.approved_at && inject.grade !== undefined && (
          <Text fw={600}>
            Graded {inject.grade}/10: {(inject.points ?? 0).toLocaleString()} points
          </Text>
        )}
        {inject.accepting && (
          <form onSubmit={submit}>
            <Stack gap="xs">
              <Textarea
                label="Note"
                description="What this submission is for"
                maxLength={2000}
                rows={3}
                value={note}
                onChange={(e) => setNote(e.currentTarget.value)}
              />
              <div>
                <Text component="label" htmlFor={fileId} fz="sm" fw={500} display="block">
                  File
                </Text>
                <input id={fileId} type="file" onChange={(e) => setFile(e.currentTarget.files?.[0] ?? null)} />
              </div>
              <Group>
                <Button type="submit" loading={saving} disabled={!file}>
                  Submit
                </Button>
              </Group>
            </Stack>
          </form>
        )}
        {outcome && (
          <Text c={outcome.ok ? 'green' : 'red'} fz="sm">
            {outcome.message}
          </Text>
        )}
      </Stack>
    </Paper>
  );
}
