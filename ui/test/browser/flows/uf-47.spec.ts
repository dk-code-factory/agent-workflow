import { expect, test } from '@playwright/test';
import { open, field, downloadText } from './harness';

test('undo host source replacement to recover a local draft', async ({ page }) => {
  const ui = await open(page); await field(page, 'Name', 'My draft');
  await page.evaluate(() => (window as any).first.update({ source: 'name: Host update\njobs: {replacement: {steps: [{argv: [echo]}]}}' }));
  await expect(ui.getByRole('button', { name: 'Inspect job replacement', exact: true })).toBeVisible();
  await expect(ui.getByLabel('Name', { exact: true })).toHaveValue('Host update');
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  await expect(ui.getByLabel('Name', { exact: true })).toHaveValue('My draft');
  expect(await downloadText(page)).toContain('name: My draft');
});
