import { expect, test } from '@playwright/test';
import { open, snapshot, field, job } from './harness';

test('configure explicit shell command', async ({ page }) => {
  const ui = await open(page); await job(page);
  await ui.getByRole('button', { name: 'Shell command', exact: true }).click();
  await field(page, 'Shell', 'bash'); await field(page, 'Command', 'echo shell');
  await field(page, 'Working directory', '/work'); await field(page, 'Timeout', '30s');
  expect((await snapshot(page)).workflow.jobs.build.steps[1]).toMatchObject({ shell: 'bash', run: 'echo shell', timeout: '30s', 'working-directory': '/work' });
});
