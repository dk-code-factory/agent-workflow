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


test('playground retains per-example drafts, history, selection, theme, and download names', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByText('Export YAML downloads your changes. Drafts stay available while this page is open. Reloading clears drafts; layout is stored separately.')).toBeVisible();
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
  await page.locator('#example').selectOption('parallel-review');
  await page.locator('#example').selectOption('classify');
  await page.getByRole('button', { name: 'Workflow properties', exact: true }).click();
  await expect(page.getByLabel('Name', { exact: true })).toHaveValue('Playground draft');
  const [download] = await Promise.all([page.waitForEvent('download'), page.getByRole('button', { name: 'Export YAML', exact: true }).click()]);
  expect(download.suggestedFilename()).toBe('classify.yml');
  const { readFile } = await import('node:fs/promises');
  expect(await readFile((await download.path())!, 'utf8')).toContain('Playground draft');
  await page.getByRole('button', { name: 'Undo', exact: true }).click();
  await expect(page.getByLabel('Name', { exact: true })).not.toHaveValue('Playground draft');
  await page.getByRole('button', { name: 'Redo', exact: true }).click();
  await expect(page.getByLabel('Name', { exact: true })).toHaveValue('Playground draft');
  expect(await page.evaluate(() => localStorage.getItem('workflow-layout:classify'))).toBe(layout);
});


test('switching preserves invalid YAML and rejected inspector fields', async ({ page }) => {
  await page.goto('/');
  await page.getByRole('button', { name: 'Inspect job classify', exact: true }).click();
  const identifier = page.getByLabel('Job ID', { exact: true });
  await identifier.fill('bad id'); await identifier.press('Tab');
  await expect(identifier).toHaveAttribute('aria-invalid', 'true');
  await page.locator('#example').selectOption('parallel-review');
  await page.locator('#theme').selectOption('dark');
  await page.locator('#example').selectOption('classify');
  await expect(page.locator('.aw-editor')).toHaveAttribute('data-theme', 'dark');
  await expect(identifier).toHaveValue('bad id');
  await expect(identifier).toHaveAttribute('aria-invalid', 'true');
  await identifier.press('Escape');
  await page.getByRole('button', { name: 'YAML', exact: true }).click();
  const yaml = page.getByRole('textbox', { name: 'Workflow YAML', exact: true });
  await yaml.press('ControlOrMeta+a');
  await page.keyboard.insertText('jobs: [broken');
  await expect(yaml).toHaveText('jobs: [broken');
  await expect(page.locator('.aw-stale')).toBeVisible();
  await page.locator('#example').selectOption('develop');
  await page.locator('#example').selectOption('classify');
  await expect(page.locator('.cm-content')).toHaveText('jobs: [broken');
  await expect(page.locator('.aw-stale')).toBeVisible();
  await page.getByRole('button', { name: 'Undo', exact: true }).click();
  await expect(page.locator('.aw-stale')).toHaveCount(0);
  await page.getByRole('button', { name: 'Redo', exact: true }).click();
  await expect(page.locator('.cm-content')).toHaveText('jobs: [broken');
});

test('reset confirms disposal, isolates examples, and retains layout and theme', async ({ page }) => {
  await page.goto('/');
  await page.getByLabel('Name', { exact: true }).fill('Classify draft');
  await page.getByLabel('Name', { exact: true }).press('Enter');
  await page.getByLabel('Name', { exact: true }).fill('Unapplied reset draft');
  page.once('dialog', dialog => dialog.dismiss());
  await page.getByRole('button', { name: 'Reset example', exact: true }).click();
  await expect(page.getByLabel('Name', { exact: true })).toHaveValue('Unapplied reset draft');
  await expect(page.locator('.cm-content')).toContainText('Classify draft');
  await expect(page.locator('.cm-content')).not.toContainText('Unapplied reset draft');
  await page.getByLabel('Name', { exact: true }).press('Escape');
  await page.getByRole('button', { name: 'Expand classify', exact: true }).click();
  await page.locator('#example').selectOption('parallel-review');
  await page.getByLabel('Name', { exact: true }).fill('Parallel draft');
  await page.getByLabel('Name', { exact: true }).press('Enter');
  await page.locator('#example').selectOption('classify');
  await page.locator('#theme').selectOption('dark');
  await page.getByRole('button', { name: 'Inspect job classify', exact: true }).click();
  await page.getByLabel('Job ID', { exact: true }).fill('bad id');
  await page.getByLabel('Job ID', { exact: true }).press('Tab');
  page.once('dialog', dialog => dialog.dismiss());
  await page.getByRole('button', { name: 'Reset example', exact: true }).click();
  await expect(page.getByLabel('Job ID', { exact: true })).toHaveValue('bad id');
  page.once('dialog', dialog => dialog.accept());
  await page.getByRole('button', { name: 'Reset example', exact: true }).click();
  await expect(page.locator('.aw-editor')).toHaveAttribute('data-theme', 'dark');
  await expect(page.getByLabel('Name', { exact: true })).not.toHaveValue('Classify draft');
  await expect(page.getByRole('button', { name: 'Expand classify', exact: true })).toHaveAttribute('aria-expanded', 'false');
  await expect(page.getByRole('button', { name: 'Undo', exact: true })).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Redo', exact: true })).toBeDisabled();
  await page.locator('#example').selectOption('parallel-review');
  await expect(page.getByLabel('Name', { exact: true })).toHaveValue('Parallel draft');
  await page.reload();
  await expect(page.getByLabel('Name', { exact: true })).not.toHaveValue('Classify draft');
});

test('detaching an example retains actual selection and displayed canvas viewport', async ({ page }) => {
  await page.goto('/');
  await page.getByRole('button', { name: 'Inspect job classify', exact: true }).click();
  await expect(page.getByLabel('Job ID', { exact: true })).toHaveValue('classify');
  const viewport = page.locator('.react-flow__viewport');
  const initial = await viewport.evaluate(element => getComputedStyle(element).transform);
  await page.getByRole('button', { name: 'Zoom In', exact: true }).click();
  await expect.poll(() => viewport.evaluate(element => getComputedStyle(element).transform)).not.toBe(initial);
  // Controls animate zoom. Observe the final displayed value before detaching.
  await expect.poll(async () => {
    const shown = await viewport.evaluate(element => getComputedStyle(element).transform);
    const layout = await page.evaluate(() => JSON.parse(localStorage.getItem('workflow-layout:classify')!).viewport);
    return Math.abs(Number(shown.match(/matrix\(([^,]+)/)?.[1]) - layout.zoom) < 0.00001;
  }).toBe(true);
  const displayed = await viewport.evaluate(element => getComputedStyle(element).transform);
  await page.locator('#example').selectOption('develop');
  await expect(page.getByLabel('Job ID', { exact: true })).toHaveCount(0);
  await page.locator('#example').selectOption('classify');
  // Do not reselect the job: the inspector and card must retain navigation.
  await expect(page.getByLabel('Job ID', { exact: true })).toHaveValue('classify');
  await expect(page.locator('.aw-card.aw-selected')).toHaveAttribute('aria-label', 'Job classify');
  await expect.poll(() => viewport.evaluate(element => getComputedStyle(element).transform)).toBe(displayed);
});
