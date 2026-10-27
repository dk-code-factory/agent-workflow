import { expect, test } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';

test('controlled React works with inline catalogs and callbacks', async ({ page }) => {
  const errors: string[] = []; page.on('pageerror', error => errors.push(error.message));
  await page.goto('/test/browser.html?controlled'); const first = page.locator('#a');
  await first.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await first.getByLabel('Name', { exact: true }).fill('Controlled job'); await page.keyboard.press('Enter');
  await expect.poll(() => page.evaluate(() => (window as any).latest?.workflow.jobs.build.name)).toBe('Controlled job');
  await first.getByRole('button', { name: 'Undo', exact: true }).click();
  await expect.poll(() => page.evaluate(() => (window as any).latest?.workflow.jobs.build.name)).toBeUndefined();
  expect(errors).toEqual([]);
});

test('editor buttons do not submit the host form', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => {
    (window as any).submits = 0;
    const form = document.createElement('form');
    const editor = document.querySelector('#a')!; editor.before(form); form.append(editor);
    form.onsubmit = event => { event.preventDefault(); (window as any).submits++; };
  });
  await first.getByRole('button', { name: 'Add job', exact: true }).click();
  await first.getByRole('button', { name: 'Duplicate job', exact: true }).click();
  await first.getByRole('button', { name: 'Zoom In', exact: true }).click();
  await first.getByLabel('Name', { exact: true }).fill('Embedded'); await page.keyboard.press('Enter');
  await first.getByRole('textbox', { name: 'Search workflow', exact: true }).press('Enter');
  expect(await page.evaluate(() => (window as any).submits)).toBe(0);
});

test('synchronous host normalization does not reenter CodeMirror updates', async ({ page }) => {
  const errors: string[] = []; page.on('pageerror', error => errors.push(error.message));
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ onChange: (snapshot: any) => {
    if (snapshot.source.includes('NORMALIZE')) (window as any).first.update({ source: snapshot.source.replaceAll('NORMALIZE', 'normalized') });
  } }));
  await first.getByRole('button', { name: 'YAML', exact: true }).click();
  await first.getByRole('textbox', { name: 'Workflow YAML' }).fill('name: NORMALIZE\njobs: {}');
  await expect(first.getByRole('textbox', { name: 'Workflow YAML' })).toContainText('NORMALIZE');
  await first.getByRole('button', { name: 'Use incoming', exact: true }).click();
  await expect(first.getByRole('textbox', { name: 'Workflow YAML' })).toContainText('normalized');
  expect(errors).toEqual([]);
});

for (const theme of ['light', 'dark', 'system'] as const) test(`accessible ${theme} theme, inspector and dialog`, async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.emulateMedia({ colorScheme: 'dark', reducedMotion: 'reduce' });
  await page.evaluate(theme => (window as any).first.update({ theme }), theme);
  await first.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  const scan = () => new AxeBuilder({ page }).include('#a').withTags(['wcag2a', 'wcag2aa', 'wcag21aa']).analyze();
  expect((await scan()).violations).toEqual([]);
  await first.getByRole('button', { name: 'Delete job', exact: true }).click();
  expect((await scan()).violations).toEqual([]);
  await first.getByRole('button', { name: 'Cancel', exact: true }).click();
  await first.getByLabel('Name', { exact: true }).fill('Local job draft');
  await page.evaluate(() => (window as any).first.update({ source: 'jobs: {}' }));
  await expect(first.getByRole('region', { name: 'Incoming source replacement' })).toBeVisible();
  expect((await scan()).violations).toEqual([]);
});

test('new jobs avoid existing positions and host validation can refresh without source edits', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ onLayoutChange: (layout: any) => { (window as any).savedLayout = layout; } }));
  for (let index = 0; index < 3; index++) await first.getByRole('button', { name: 'Add job', exact: true }).click();
  const positions = await page.evaluate(() => Object.values((window as any).savedLayout.positions) as { x: number; y: number }[]);
  for (let index = 0; index < positions.length; index++) for (let other = index + 1; other < positions.length; other++) {
    const a = positions[index]; const b = positions[other];
    expect(a.x + 280 <= b.x || b.x + 280 <= a.x || a.y + 140 <= b.y || b.y + 140 <= a.y).toBe(true);
  }
  await page.goto('/test/browser.html?controlled');
  await expect(page.locator('#a').getByRole('button', { name: 'Inspect job build', exact: true })).toBeVisible();
  const source = await page.evaluate(() => (window as any).latest.source);
  await page.evaluate(() => (window as any).changePolicy());
  await expect.poll(() => page.evaluate(() => (window as any).latest.diagnostics.some((item: any) => item.code === 'policy'))).toBe(true);
  expect(await page.evaluate(() => (window as any).latest.source)).toBe(source);
});
