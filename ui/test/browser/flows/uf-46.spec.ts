import { expect, test } from '@playwright/test';
import { open, field, downloadText } from './harness';

test('preserve handwritten metadata and export a focused form edit', async ({ page }) => {
  const source = `# retained header
name: Original
custom: keep
jobs:
  build:
    # retained step
    steps:
      - id: greet
        uses: agent/run
        with:
          prompt: |
            Line one
            Line two
          agent: claude.toml
`;
  const ui = await open(page, source); await ui.getByRole('button', { name: 'Split', exact: true }).click();
  await field(page, 'Name', 'Renamed');
  const exported = await downloadText(page);
  expect(exported).toContain('# retained header'); expect(exported).toContain('# retained step');
  expect(exported).toContain('custom: keep'); expect(exported).toContain('Line two');
  expect(exported.indexOf('name:')).toBeLessThan(exported.indexOf('jobs:'));
  expect(exported).toContain('name: Renamed');
});
