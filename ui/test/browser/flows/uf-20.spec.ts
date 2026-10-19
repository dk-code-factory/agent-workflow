import { expect, test } from '@playwright/test';
import { open, snapshot, field, job } from './harness';

test('use host supplied action fields and renderer', async ({ page }) => {
  const ui = await open(page); await job(page);
  await ui.getByRole('button', { name: 'Custom Claude', exact: true }).click();
  await field(page, 'Custom model', 'host-model');
  await ui.getByRole('button', { name: 'Custom prompt', exact: true }).click();
  await expect(ui.getByLabel('Prompt', { exact: true })).toHaveValue('Custom value');
  expect((await snapshot(page)).workflow.jobs.build.steps[1].with).toEqual({ model: 'host-model', prompt: 'Custom value' });
});
