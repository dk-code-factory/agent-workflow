import { expect, test } from '@playwright/test';
import { open, snapshot, field, step } from './harness';

test('replace advanced parameters and reflect them in guided fields', async ({ page }) => {
  const ui = await open(page); await step(page);
  await ui.getByText('Advanced parameters', { exact: true }).click();
  await field(page, 'Parameters (YAML)', 'prompt: Updated\nmodel: revised');
  await expect(ui.getByLabel('Prompt', { exact: true })).toHaveValue('Updated');
  await expect(ui.getByLabel('Model', { exact: true })).toHaveValue('revised');
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with).toEqual({ prompt: 'Updated', model: 'revised' });
});
