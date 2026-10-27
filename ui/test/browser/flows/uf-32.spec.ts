import { expect, test } from '@playwright/test';

test('stale preview discards pending property drafts before editing resumes', async ({ page }) => {
  await page.goto('/test/browser.html'); const editor = page.locator('#a');
  await editor.getByLabel('Name', { exact: true }).fill('Uncommitted name');
  const source = await page.evaluate(() => (window as any).first.getSnapshot().source);
  await page.evaluate(() => (window as any).first.update({ source: 'jobs: [' }));
  await expect(editor.getByLabel('Name', { exact: true })).toHaveValue('Uncommitted name');
  await editor.getByRole('button', { name: 'Use incoming', exact: true }).click();
  await expect(editor.getByLabel('Name', { exact: true })).toHaveAttribute('readonly', '');
  await expect(editor.getByLabel('Name', { exact: true })).toHaveValue('Browser workflow');
  await page.evaluate(source => (window as any).first.update({ source }), source);
  await editor.getByLabel('Name', { exact: true }).press('Tab');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.name)).toBe('Browser workflow');
});


test('repair malformed source while retaining the last preview', async ({ page }) => {
  await page.goto('/test/browser.html'); const ui = page.locator('#a');
  await ui.getByRole('button', { name: 'Split', exact: true }).click();
  const yaml = ui.getByRole('textbox', { name: 'Workflow YAML' });
  const source = await page.evaluate(() => (window as any).first.getSnapshot().source);
  await yaml.fill('jobs: [');
  await expect(ui.getByText('Stale preview', { exact: false })).toBeVisible();
  await expect(ui.getByRole('button', { name: 'Inspect job build', exact: true })).toBeVisible();
  await expect(ui.getByRole('button', { name: 'Add job', exact: true })).toBeDisabled();
  await expect(yaml).toContainText('jobs: [');
  await yaml.fill(source);
  await expect(ui.getByText('Stale preview', { exact: false })).toHaveCount(0);
  await expect(ui.getByRole('button', { name: 'Add job', exact: true })).toBeEnabled();
});
