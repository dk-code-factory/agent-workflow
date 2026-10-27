import { expect, test } from '@playwright/test';
import { open, snapshot, field, step } from './harness';

test('replace advanced parameters and reflect them in guided fields', async ({ page }) => {
  const ui = await open(page); await step(page);
  await ui.getByText('Advanced parameters', { exact: true }).click();
  await expect(ui.getByText('Applying this YAML replaces the entire mapping', { exact: false })).toBeVisible();
  await field(page, 'Parameters (YAML)', 'prompt: Updated\nmodel: revised');
  await expect(ui.getByLabel('Prompt', { exact: true })).toHaveValue('Updated');
  await expect(ui.getByLabel('Model', { exact: true })).toHaveValue('revised');
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with).toEqual({ prompt: 'Updated', model: 'revised' });
});

test('numeric custom fields store numbers and guided edits retain other parameters and comments', async ({ page }) => {
  const ui = await open(page, 'jobs:\n  build:\n    steps:\n      - id: greet\n        uses: action/custom\n        with:\n          title: before\n          retries: 1\n          imported: [keep] # preserve parameter\n');
  await page.evaluate(() => (window as any).first.update({ actions: [{ uses: 'action/custom', label: 'Custom', fields: [
    { key: 'title', label: 'Title', type: 'text' }, { key: 'retries', label: 'Retries', type: 'number' },
  ] }] }));
  await step(page);
  await page.evaluate(() => (window as any).first.update({ onSave: () => { (window as any).saved = true; } }));
  await ui.getByLabel('Retries', { exact: true }).fill('1e309');
  await ui.getByLabel('Retries', { exact: true }).press('Tab');
  await expect(ui.getByLabel('Retries', { exact: true })).toHaveAttribute('aria-invalid', 'true');
  await ui.getByRole('button', { name: 'Save', exact: true }).click();
  expect(await page.evaluate(() => (window as any).saved ?? false)).toBe(false);
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.retries).toBe(1);
  await ui.getByLabel('Retries', { exact: true }).press('Escape');
  await expect(ui.getByLabel('Retries', { exact: true })).toHaveValue('1');
  await field(page, 'Retries', '2.5');
  await field(page, 'Title', 'after');
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with).toEqual({ title: 'after', retries: 2.5, imported: ['keep'] });
  expect((await snapshot(page)).source).toContain('# preserve parameter');
});
