import { createTheme, type MantineColorsTuple } from '@mantine/core';

// The single source of the app's palette, type and spacing. Components use
// these through Mantine props and --mantine-* CSS variables, never their own
// colour values.

// A deep navy in place of Mantine's neutral greys for the dark scheme: dark[7]
// is the page, dark[6] cards, dark[4] borders and diagram lines, dark[0] text.
const navy: MantineColorsTuple = [
  '#c9d1e0',
  '#a7b2c7',
  '#8592ab',
  '#5f6d88',
  '#3a4760',
  '#2a3550',
  '#18213a',
  '#0f1626',
  '#0b1120',
  '#070c16',
];

export const theme = createTheme({
  primaryColor: 'cyan',
  colors: { dark: navy },
  // System fonts only: the scoring box may have no internet to fetch any.
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif',
  fontFamilyMonospace: 'ui-monospace, "SF Mono", "Cascadia Mono", Menlo, Consolas, monospace',
  headings: { fontWeight: '700' },
  defaultRadius: 'md',
});
