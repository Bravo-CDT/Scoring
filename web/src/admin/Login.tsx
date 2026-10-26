import { Alert, Button, Center, Paper, PasswordInput, Stack, TextInput, Title } from '@mantine/core';
import { type FormEvent, useState } from 'react';
import { describeError } from '../api/client';

interface LoginProps {
  /** The admin session's login; rejects with the API's error on bad credentials. */
  onLogin: (username: string, password: string) => Promise<void>;
  /** Whether the previous session ended because its token stopped working. */
  sessionExpired: boolean;
  /** The form's heading. */
  title?: string;
}

/** A login form: the admin's, or with another title, the blue team's. */
export function Login({ onLogin, sessionExpired, title = 'Admin login' }: LoginProps) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  async function submit(event: FormEvent) {
    event.preventDefault();
    setSubmitting(true);
    setError(null);
    try {
      await onLogin(username, password);
      // On success the admin view replaces this form.
    } catch (err) {
      setError(describeError(err));
      setSubmitting(false);
    }
  }

  return (
    <Center h="100vh" px="md">
      <Paper withBorder p="xl" radius="md" w="100%" maw={380}>
        <form onSubmit={submit}>
          <Stack>
            <Title order={1} size="h2">
              {title}
            </Title>
            {sessionExpired && !error && (
              <Alert color="yellow">Your session has expired or was ended. Log in again to continue.</Alert>
            )}
            {error && (
              <Alert color="red" title="Login failed">
                {error}
              </Alert>
            )}
            <TextInput
              label="Username"
              autoComplete="username"
              required
              value={username}
              onChange={(e) => setUsername(e.currentTarget.value)}
            />
            <PasswordInput
              label="Password"
              autoComplete="current-password"
              required
              value={password}
              onChange={(e) => setPassword(e.currentTarget.value)}
            />
            <Button type="submit" loading={submitting}>
              Log in
            </Button>
          </Stack>
        </form>
      </Paper>
    </Center>
  );
}
