import { expect, test } from '@playwright/test';

test('all repository workflows load in the playground and export unchanged source', async ({ page }) => {
  const errors: string[] = []; page.on('pageerror', error => errors.push(error.message));
  await page.goto('/');
  const examples = ['classify', 'parallel-review', 'develop', 'develop-iteration'];
  for (const example of examples) {
    await page.locator('#example').selectOption(example);
    await expect(page.locator('.aw-card')).not.toHaveCount(0);
    await expect(page.locator('.aw-stale')).toHaveCount(0);
    const [download] = await Promise.all([page.waitForEvent('download'), page.getByRole('button', { name: 'Export YAML', exact: true }).click()]);
    const path = await download.path();
    const { readFile } = await import('node:fs/promises');
    const { resolve } = await import('node:path');
    expect(await readFile(path!, 'utf8')).toBe(await readFile(resolve(import.meta.dirname, '../../../../workflows', `${example}.yml`), 'utf8'));
  }
  expect(errors).toEqual([]);
});


test('playground downloads use example names without claiming draft persistence', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByText('Export YAML downloads your changes. Only layout is stored; workflow drafts are not retained.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Save', exact: true })).toHaveCount(0);
  await expect(page.getByRole('status', { name: 'Save status', exact: true })).toHaveCount(0);
  for (const theme of ['dark', 'light', 'system']) {
    await page.locator('#theme').selectOption(theme);
    await expect(page.locator('.aw-editor')).toHaveAttribute('data-theme', theme);
  }
  await page.getByRole('button', { name: 'Auto layout', exact: true }).click();
  const layout = await page.evaluate(() => localStorage.getItem('workflow-layout:classify'));
  expect(layout).toBeTruthy();
  await page.getByRole('button', { name: 'Workflow properties', exact: true }).click();
  await page.getByLabel('Name', { exact: true }).fill('Playground draft');
  await page.getByLabel('Name', { exact: true }).press('Enter');
  const [download] = await Promise.all([page.waitForEvent('download'), page.getByRole('button', { name: 'Export YAML', exact: true }).click()]);
  expect(download.suggestedFilename()).toBe('classify.yml');
  const { readFile } = await import('node:fs/promises');
  expect(await readFile((await download.path())!, 'utf8')).toContain('Playground draft');
  await page.locator('#example').selectOption('parallel-review');
  await page.locator('#example').selectOption('classify');
  await page.getByRole('button', { name: 'Workflow properties', exact: true }).click();
  await expect(page.getByLabel('Name', { exact: true })).not.toHaveValue('Playground draft');
  expect(await page.evaluate(() => localStorage.getItem('workflow-layout:classify'))).toBe(layout);
});
