import { expect, test } from '@playwright/test';

test('pending deletion is invalidated by a host replacement', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await first.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await first.getByRole('button', { name: 'Delete job', exact: true }).click();
  await page.evaluate(() => (window as any).first.update({ source: 'jobs: {replacement: {steps: [{argv: [echo]}]}}' }));
  await expect(first.getByRole('alertdialog')).toHaveCount(0);
  await expect(first.getByRole('alert')).toContainText('workflow changed');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.replacement)).toBeTruthy();
});

test('deletion shows impact, confirms and removes structural dependencies', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await first.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await first.getByRole('button', { name: 'Delete job', exact: true }).click();
  await expect(first.getByRole('alertdialog')).toContainText('jobs.test.needs');
  const source = await page.evaluate(() => (window as any).first.getSnapshot().source);
  await first.getByRole('button', { name: 'Cancel', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().source)).toBe(source);
  await first.getByRole('button', { name: 'Delete job', exact: true }).click();
  await first.getByRole('button', { name: 'Confirm delete', exact: true }).click();
  await expect(first.getByRole('button', { name: 'Inspect job build', exact: true })).toHaveCount(0);
  await expect(first.getByRole('button', { name: 'Inspect job test', exact: true })).toBeVisible();
});
