import { expect, test } from '@playwright/test';
for (const [consumer, job] of [['react', 'packed'], ['standalone', 'standalone']]) test(`packed ${consumer} artifact`, async ({ page }) => {
  const errors: string[] = []; page.on('pageerror', e => errors.push(e.message));
  await page.goto(`/packed/${consumer}/index.html`);
  await expect(page.getByRole('button', { name: `Inspect job ${job}`, exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Add job', exact: true }).click();
  await expect(page.getByLabel('Job ID', { exact: true })).toHaveValue('job');
  expect(errors).toEqual([]);
});
