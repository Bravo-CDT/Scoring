import { Alert, Badge, Center, Group, Loader, Modal, Paper, Stack, Text } from '@mantine/core';
import { useEffect, useState } from 'react';
import { type CheckResult, getResults } from '../api/client';

/** How many of the service's latest checks the history shows. */
const HISTORY_LIMIT = 50;

type HistoryState =
  | { kind: 'loading' }
  | { kind: 'error'; error: Error }
  | { kind: 'loaded'; results: CheckResult[] };

/**
 * A modal listing one service's latest check results, newest first, each
 * with the check's explanation of its outcome when it gave one. That
 * explanation comes from the scored service itself and is untrusted, so it is
 * only ever rendered as text.
 *
 * The history is fetched once, when this mounts; give it a `key` of the
 * service's name so switching services starts it afresh.
 */
export function ServiceHistory({ name, onClose }: { name: string; onClose: () => void }) {
  const [state, setState] = useState<HistoryState>({ kind: 'loading' });

  useEffect(() => {
    let cancelled = false;
    getResults(name, HISTORY_LIMIT).then(
      (results) => {
        if (!cancelled) setState({ kind: 'loaded', results });
      },
      (error: unknown) => {
        if (!cancelled) setState({ kind: 'error', error: error instanceof Error ? error : new Error(String(error)) });
      },
    );
    return () => {
      cancelled = true;
    };
  }, [name]);

  return (
    <Modal opened onClose={onClose} title={`${name}: recent checks`} size="lg" closeButtonProps={{ 'aria-label': 'Close' }}>
      <HistoryBody state={state} />
    </Modal>
  );
}

function HistoryBody({ state }: { state: HistoryState }) {
  switch (state.kind) {
    case 'loading':
      return (
        <Center py="md">
          <Loader />
        </Center>
      );
    case 'error':
      return (
        <Alert color="red" title="Cannot load the history">
          {state.error.message}
        </Alert>
      );
    case 'loaded':
      if (state.results.length === 0) {
        return <Text c="dimmed">No checks yet.</Text>;
      }
      return (
        <Stack component="ul" gap="xs" m={0} p={0} style={{ listStyle: 'none' }}>
          {state.results.map((result) => (
            <ResultItem key={result.id} result={result} />
          ))}
        </Stack>
      );
  }
}

function ResultItem({ result }: { result: CheckResult }) {
  return (
    <Paper component="li" withBorder px="md" py="xs" radius="md">
      <Group justify="space-between">
        <Group gap="sm">
          <Text fw={500}>Round {result.round}</Text>
          <Badge color={result.success ? 'green' : 'red'} variant="filled">
            {result.success ? 'Up' : 'Down'}
          </Badge>
        </Group>
        <Text c="dimmed" size="sm">
          {new Date(result.timestamp).toLocaleTimeString()}
          {result.success && ` · ${result.latency_ms} ms`}
        </Text>
      </Group>
      {result.detail && (
        // Plain JSX text: React escapes it, so markup in it shows as written.
        <Text size="sm" ff="monospace" mt={4} style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>
          {result.detail}
        </Text>
      )}
    </Paper>
  );
}
