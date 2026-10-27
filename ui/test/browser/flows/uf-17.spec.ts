import { expect, test } from '@playwright/test';
import { open, snapshot, field, job } from './harness';

test('configure direct command argv working directory and timeout', async ({ page }) => {
  const ui = await open(page); await job(page);
  await ui.getByRole('button', { name: 'Direct command', exact: true }).click();
  await field(page, 'Arguments (YAML array)', '[echo, reviewed]');
  await field(page, 'Working directory', '/workspace'); await field(page, 'Timeout', '2m');
  expect((await snapshot(page)).workflow.jobs.build.steps[1]).toMatchObject({ argv: ['echo', 'reviewed'], 'working-directory': '/workspace', timeout: '2m' });
  await expect(ui.getByRole('button', { name: 'step direct command', exact: true })).toBeVisible();
});
