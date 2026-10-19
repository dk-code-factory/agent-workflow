import { expect, test } from '@playwright/test';
import { open, snapshot, downloadText } from './harness';

test('download original bytes and current invalid drafts', async ({ page }) => {
  await open(page); const original = (await snapshot(page)).source;
  expect(await downloadText(page)).toBe(original);
  await page.locator('#a').getByRole('button', { name: 'YAML', exact: true }).click();
  await page.getByRole('textbox', { name: 'Workflow YAML' }).first().fill('jobs: [');
  expect(await downloadText(page)).toBe('jobs: [');
});
