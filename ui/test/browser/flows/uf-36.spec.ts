import { expect, test } from '@playwright/test';

test('read-only viewer retains inspection, expansion and source navigation', async ({ page }) => {
  await page.goto('/test/browser.html?viewer'); const first = page.locator('#a');
  await expect(first.getByRole('button', { name: 'Add job', exact: true })).toHaveCount(0);
  await first.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await expect(first.getByLabel('Job ID', { exact: true })).toHaveAttribute('readonly', '');
  await expect(first.getByRole('button', { name: 'Delete job', exact: true })).toBeDisabled();
  await first.getByRole('button', { name: 'Expand build', exact: true }).click();
  await expect(first.getByRole('button', { name: 'Expand build', exact: true })).toHaveAttribute('aria-expanded', 'false');
  await first.getByRole('button', { name: 'YAML', exact: true }).click();
  await expect(first.getByRole('textbox', { name: 'Workflow YAML' })).toHaveAttribute('contenteditable', 'false');
  const text = await page.evaluate(() => (window as any).first.getSnapshot().source);
  expect(text).toContain('# Fixture');
});
