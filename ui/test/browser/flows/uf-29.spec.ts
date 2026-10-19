import { expect, test } from '@playwright/test';
import { open, snapshot } from './harness';

test('paste source and retain unfamiliar fields while updating the graph', async ({ page }) => {
  const ui = await open(page);
  await ui.getByRole('button', { name: 'Split', exact: true }).click();
  const source = '# handwritten\nname: Pasted\ncustom: retained\njobs: {pasted: {steps: [{argv: [echo]}]}}';
  await ui.getByRole('textbox', { name: 'Workflow YAML' }).fill(source);
  await expect(ui.getByRole('button', { name: 'Inspect job pasted', exact: true })).toBeVisible();
  expect((await snapshot(page)).source).toBe(source);
  expect((await snapshot(page)).workflow.custom).toBe('retained');
});
