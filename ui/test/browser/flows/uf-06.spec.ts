import { expect, test } from '@playwright/test';

test('empty source has a guided starting point and preserves undo', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: '' }));
  await first.getByRole('button', { name: 'Create workflow', exact: true }).click();
  await expect(first.getByLabel('Job ID', { exact: true })).toHaveValue('job');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().diagnostics.filter((d: any) => d.severity === 'error'))).toEqual([]);
  await first.getByRole('button', { name: 'Undo', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().source)).toBe('');
});
