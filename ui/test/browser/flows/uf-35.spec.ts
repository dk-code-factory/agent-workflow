import { expect, test } from '@playwright/test';
import { open, snapshot, downloadText } from './harness';

test('download original bytes and current invalid drafts', async ({ page }) => {
  await open(page); const original = (await snapshot(page)).source;
  expect(await downloadText(page)).toBe(original);
  await page.locator('#a').getByRole('button', { name: 'YAML', exact: true }).click();
  await page.getByRole('textbox', { name: 'Workflow YAML' }).first().fill('jobs: [');
  expect(await downloadText(page)).toBe('jobs: [');
});

test('exporting a changed draft does not mark it as saved by the host', async ({ page }) => {
  const editor = await open(page);
  await page.evaluate(() => (window as any).first.update({ onSave: () => {} }));
  const status = editor.getByRole('status', { name: 'Save status', exact: true });
  await editor.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(status).toHaveText('Saved');
  await editor.getByRole('button', { name: 'YAML', exact: true }).click();
  await page.getByRole('textbox', { name: 'Workflow YAML' }).first().fill('jobs: [');
  expect(await downloadText(page)).toBe('jobs: [');
  await expect(status).toHaveText('Unsaved changes');
  await editor.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(status).toHaveText('Saved');
});
