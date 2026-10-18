import { MantineProvider } from '@mantine/core';
import { render as rtlRender } from '@testing-library/react';
import type { ReactElement } from 'react';

/** Renders `ui` inside the MantineProvider every Mantine component needs. */
export function render(ui: ReactElement) {
  return rtlRender(ui, {
    wrapper: ({ children }) => <MantineProvider>{children}</MantineProvider>,
  });
}
