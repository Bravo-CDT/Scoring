import { Alert, Loader, Modal, Table } from '@mantine/core';
import { useEffect, useState } from 'react';
import { describeError, getScoreBreakdown, type TeamBreakdown } from '../api/client';

/**
 * Where a team's points came from, in a dialog, fetched when it opens; mount
 * it to open it. Per-inject points stay on the team's own injects page, so
 * this shows only their total.
 */
export function ScoreBreakdown({
  team,
  label,
  onClose,
}: {
  team: 'blue' | 'red';
  label: string;
  onClose: () => void;
}) {
  const [breakdown, setBreakdown] = useState<TeamBreakdown | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    getScoreBreakdown().then(
      (b) => {
        if (cancelled) return;
        setBreakdown(b[team]);
        setError(null);
      },
      (err: unknown) => !cancelled && setError(describeError(err)),
    );
    return () => {
      cancelled = true;
    };
  }, [team]);

  const rows: [string, number][] = breakdown
    ? [
        ...(team === 'blue' ? [['Starting score', breakdown.starting] as [string, number]] : []),
        ['Services', breakdown.services],
        ['Injects', breakdown.injects],
        ['Manual adjustments', breakdown.manual],
        ['Scavenger hunt', breakdown.scavenger_hunt],
      ]
    : [];

  return (
    <Modal opened onClose={onClose} title={`${label} score`} closeButtonProps={{ 'aria-label': 'Close' }}>
      {error && <Alert color="red">{error}</Alert>}
      {!breakdown && !error && <Loader />}
      {breakdown && (
        <Table>
          <Table.Tbody>
            {rows.map(([name, points]) => (
              <Table.Tr key={name}>
                <Table.Td>{name}</Table.Td>
                <Table.Td ta="right">{points.toLocaleString()}</Table.Td>
              </Table.Tr>
            ))}
            <Table.Tr fw={700}>
              <Table.Td>Total</Table.Td>
              <Table.Td ta="right">{breakdown.total.toLocaleString()}</Table.Td>
            </Table.Tr>
          </Table.Tbody>
        </Table>
      )}
    </Modal>
  );
}
