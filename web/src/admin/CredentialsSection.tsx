import { Alert, Badge, Paper, Stack, Table, Text, Title } from '@mantine/core';
import { useEffect, useState } from 'react';
import { type AuthedFetch, type Credentials, describeError, getCredentials, resetCredential } from '../api/client';
import { changedLabel } from '../credentials/changedLabel';
import { ConfirmButton } from './ConfirmButton';

/** How often the list is refetched, to show blue team changes as they happen. */
const refreshMs = 10_000;

/**
 * The credentials each credentialed check logs in with, as the blue team has
 * left them, with a per-service reset to the config's defaults (for a box
 * that has been rebuilt).
 */
export function CredentialsSection({ send }: { send: AuthedFetch }) {
  const [credentials, setCredentials] = useState<Credentials | null>(null);
  const [error, setError] = useState<string | null>(null);
  // Kept apart from error, which each successful poll clears.
  const [resetError, setResetError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    async function load() {
      try {
        const loaded = await getCredentials(send);
        if (!cancelled) {
          setCredentials(loaded);
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

  async function reset(service: string) {
    setBusy(service);
    setResetError(null);
    try {
      const entry = await resetCredential(send, service);
      setCredentials((c) =>
        c && { ...c, credentials: c.credentials.map((e) => (e.service === entry.service ? entry : e)) },
      );
    } catch (err) {
      setResetError(`Reset ${service} failed: ${describeError(err)}`);
    } finally {
      setBusy(null);
    }
  }

  const entries = credentials?.credentials ?? [];
  const changed = entries.filter((e) => e.changed_at).length;
  return (
    <Paper withBorder p="md" radius="md">
      <Stack gap="sm">
        <Title order={2} size="h4">
          Credentials
        </Title>
        <Text c="dimmed" fz="sm">
          {changed} of {entries.length} changed by the blue team.
        </Text>
        {error && <Alert color="red">{error}</Alert>}
        {resetError && <Alert color="red">{resetError}</Alert>}
        <Table>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Service</Table.Th>
              <Table.Th>Box</Table.Th>
              <Table.Th>User</Table.Th>
              <Table.Th />
              <Table.Th />
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {entries.map((entry) => (
              <Table.Tr key={entry.service} aria-label={entry.service}>
                <Table.Td>{entry.service}</Table.Td>
                <Table.Td>{entry.host_name ?? entry.host}</Table.Td>
                <Table.Td>{entry.username}</Table.Td>
                <Table.Td>
                  <Badge color={entry.changed_at ? 'cyan' : 'gray'} variant="light">
                    {changedLabel(entry.changed_at)}
                  </Badge>
                </Table.Td>
                <Table.Td>
                  <ConfirmButton
                    size="xs"
                    variant="default"
                    disabled={!entry.changed_at || busy !== null}
                    loading={busy === entry.service}
                    aria-label={`Reset ${entry.service} to default`}
                    confirmTitle={`Reset ${entry.service}?`}
                    confirmMessage={`${entry.service}'s check goes back to the user and password in the config from the next round.`}
                    confirmLabel="Reset to default"
                    onConfirm={() => void reset(entry.service)}
                  >
                    Reset to default
                  </ConfirmButton>
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </Stack>
    </Paper>
  );
}
