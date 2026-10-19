import { expect, test } from '@playwright/test';
import { open, snapshot, job } from './harness';

test('append templates only to sequential jobs', async ({ page }) => {
  const ui = await open(page);
  await expect(ui.getByRole('button', { name: 'Direct command', exact: true })).toBeDisabled();
  await job(page); await ui.getByRole('button', { name: 'Direct command', exact: true }).click();
  await expect(ui.getByLabel('Step ID', { exact: true })).toHaveValue('step');
  await job(page); await ui.getByRole('button', { name: 'Add step', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps.map((s: any) => s.id)).toEqual(['greet', 'step', 'step_2']);
  await job(page); await ui.getByLabel('Job kind', { exact: true }).selectOption('reusable');
  await expect(ui.getByRole('button', { name: 'Direct command', exact: true })).toBeDisabled();
});
