import { expect, test } from '@playwright/test';
import { open, snapshot, field, job, step } from './harness';

test('configure job and step conditions and error handling', async ({ page }) => {
  const ui = await open(page); await job(page);
  await field(page, 'Condition', 'true'); await ui.getByRole('checkbox', { name: 'Continue on error', exact: true }).check();
  await step(page); await field(page, 'Condition', 'false');
  await ui.getByRole('checkbox', { name: 'Continue on error', exact: true }).check();
  const build = (await snapshot(page)).workflow.jobs.build;
  expect(build).toMatchObject({ if: 'true', 'continue-on-error': true, steps: [{ if: 'false', 'continue-on-error': true }] });
  await expect(ui.getByRole('article', { name: 'Job build', exact: true })).toContainText('continue on error');
});
