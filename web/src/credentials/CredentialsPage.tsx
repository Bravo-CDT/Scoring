import {
  Alert,
  Anchor,
  Badge,
  Button,
  Container,
  Group,
  Loader,
  NativeSelect,
  Paper,
  PasswordInput,
  Stack,
  Text,
  Title,
} from '@mantine/core';
import { type FormEvent, useCallback, useEffect, useState } from 'react';
import {
  type AuthedFetch,
  type CredentialEntry,
  type Credentials,
  describeError,
  getCredentials,
  setCredential,
} from '../api/client';
import { Login } from '../admin/Login';
import { useBlueSession } from '../admin/useAdminSession';
import { parseIPv4 } from '../scoreboard/topology';
import { changedLabel } from './changedLabel';

/**
 * The blue team's credentials page: for each service whose check logs in,
 * which user it logs in as, and a form to switch it to another allowed user
 * with a new password.
 */
export function CredentialsPage() {
  const session = useBlueSession();
  if (session.token === null) {
    return <Login onLogin={session.login} sessionExpired={session.sessionExpired} title="Blue team login" />;
  }
  return <CredentialsList send={session.authedFetch} onLogout={session.logout} />;
}

function CredentialsList({ send, onLogout }: { send: AuthedFetch; onLogout: () => void }) {
  const [credentials, setCredentials] = useState<Credentials | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    getCredentials(send).then(
      (c) => !cancelled && setCredentials(c),
      (err: unknown) => !cancelled && setError(describeError(err)),
    );
    return () => {
      cancelled = true;
    };
  }, [send]);

  const replace = useCallback((entry: CredentialEntry) => {
    setCredentials((c) =>
      c && { ...c, credentials: c.credentials.map((e) => (e.service === entry.service ? entry : e)) },
    );
  }, []);

  return (
    <Container size="sm" py="xl">
      <Stack gap="lg">
        <Group justify="space-between" align="baseline">
          <Title order={1}>Credentials</Title>
          <Group gap="md">
            <Anchor href="/" fz="sm">
              Scoreboard
            </Anchor>
            <Button variant="subtle" size="xs" onClick={onLogout}>
              Log out
            </Button>
          </Group>
        </Group>
        <Text c="dimmed">
          The user and password each score check logs in with. A change is used from the next round on.
        </Text>
        {error && <Alert color="red">{error}</Alert>}
        {credentials === null && !error && <Loader />}
        {credentials?.credentials.length === 0 && <Text c="dimmed">No score checks log in to anything.</Text>}
        {credentials &&
          byHost(credentials.credentials).map(([host, entries]) => (
            <Paper key={host} withBorder p="md">
              <Group gap="xs" align="baseline" mb="sm">
                <Text fw={700} fz="lg">
                  {entries[0].host_name ?? entries[0].host}
                </Text>
                {entries[0].host_name && (
                  <Text c="dimmed" ff="monospace">
                    {entries[0].host}
                  </Text>
                )}
              </Group>
              <Stack gap="md">
                {entries.map((entry) => (
                  <CredentialRow
                    key={entry.service}
                    entry={entry}
                    allowedUsers={credentials.allowed_users}
                    send={send}
                    onSaved={replace}
                  />
                ))}
              </Stack>
            </Paper>
          ))}
      </Stack>
    </Container>
  );
}

/** Entries grouped by host, hosts in address order. */
function byHost(entries: CredentialEntry[]): [string, CredentialEntry[]][] {
  const groups = new Map<string, CredentialEntry[]>();
  for (const entry of entries) {
    groups.set(entry.host, [...(groups.get(entry.host) ?? []), entry]);
  }
  return [...groups].sort(
    ([a], [b]) => (parseIPv4(a) ?? Infinity) - (parseIPv4(b) ?? Infinity) || a.localeCompare(b),
  );
}

function CredentialRow({
  entry,
  allowedUsers,
  send,
  onSaved,
}: {
  entry: CredentialEntry;
  allowedUsers: string[];
  send: AuthedFetch;
  onSaved: (entry: CredentialEntry) => void;
}) {
  const [username, setUsername] = useState(entry.username);
  const [password, setPassword] = useState('');
  const [saving, setSaving] = useState(false);
  const [outcome, setOutcome] = useState<{ ok: boolean; message: string } | null>(null);
  // The current user stays choosable even if it has since left the list.
  const users = allowedUsers.includes(entry.username) ? allowedUsers : [entry.username, ...allowedUsers];

  async function save(event: FormEvent) {
    event.preventDefault();
    setSaving(true);
    setOutcome(null);
    try {
      onSaved(await setCredential(send, entry.service, username, password));
      setPassword('');
      setOutcome({ ok: true, message: 'Saved' });
    } catch (err) {
      setOutcome({ ok: false, message: describeError(err) });
    } finally {
      setSaving(false);
    }
  }

  return (
    <form onSubmit={save} role="group" aria-label={entry.service}>
      <Group justify="space-between" mb={4}>
        <Group gap="xs">
          <Text fw={600}>{entry.service}</Text>
          <Text c="dimmed">as {entry.username}</Text>
        </Group>
        <Badge color={entry.changed_at ? 'cyan' : 'gray'} variant="light">
          {changedLabel(entry.changed_at)}
        </Badge>
      </Group>
      <Group align="flex-end" gap="sm" wrap="wrap">
        <NativeSelect
          label="User"
          data={users}
          value={username}
          onChange={(e) => setUsername(e.currentTarget.value)}
        />
        <PasswordInput
          label="New password"
          autoComplete="new-password"
          required
          value={password}
          onChange={(e) => setPassword(e.currentTarget.value)}
          style={{ flex: 1, minWidth: '12rem' }}
        />
        <Button type="submit" loading={saving}>
          Save
        </Button>
      </Group>
      {outcome && (
        <Text c={outcome.ok ? 'green' : 'red'} fz="sm" mt={4}>
          {outcome.message}
        </Text>
      )}
    </form>
  );
}
