import { Button, type ButtonProps, Group, Modal, Stack, Text } from '@mantine/core';
import { type ReactNode, useState } from 'react';

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
export function ConfirmButton({
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
