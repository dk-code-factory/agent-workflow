import { expect, test } from '@playwright/test';
import { open, field, downloadText, snapshot, step } from './harness';

const incoming = 'name: Host update\njobs: {replacement: {steps: [{argv: [echo]}]}}';

test('conflicting replacements wait for a choice and remain undoable', async ({ page }) => {
  const ui = await open(page); await field(page, 'Name', 'My draft');
  await page.evaluate(source => (window as any).first.update({ source }), incoming);
  const notice = ui.getByRole('region', { name: 'Incoming source replacement' });
  await expect(notice).toBeVisible();
  await expect(ui.getByLabel('Name', { exact: true })).toHaveValue('My draft');
  await notice.getByRole('button', { name: 'Keep local', exact: true }).click();
  await page.evaluate(source => (window as any).first.update({ source }), incoming);
  await expect(notice).toHaveCount(0);
  expect(await downloadText(page)).toContain('name: My draft');
  await page.evaluate(source => (window as any).first.update({ source }), incoming.replace('Host update', 'Newest update'));
  await notice.getByRole('button', { name: 'Use incoming', exact: true }).click();
  await expect(ui.getByLabel('Name', { exact: true })).toHaveValue('Newest update');
  expect((await snapshot(page)).hasLocalChanges).toBe(false);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  await expect(ui.getByLabel('Name', { exact: true })).toHaveValue('My draft');
});

for (const resolveWith of ['button', 'handle']) {
  test(`${resolveWith} discards unapplied fields without applying them`, async ({ page }) => {
    const ui = await open(page); await step(page);
    const prompt = ui.getByLabel('Prompt', { exact: true });
    await prompt.fill('Unapplied prompt');
    const before = await snapshot(page);
    await page.evaluate(source => (window as any).first.update({ source }), before.source.replace('Hello', 'Incoming prompt'));
    await expect(ui.getByRole('region', { name: 'Incoming source replacement' })).toBeVisible();
    if (resolveWith === 'button') await ui.getByRole('button', { name: 'Use incoming', exact: true }).click();
    else await page.evaluate(() => (window as any).first.resolveSourceReplacement('use-incoming'));
    await expect(prompt).toHaveValue('Incoming prompt');
    await prompt.press('Tab');
    expect((await snapshot(page)).source).not.toContain('Unapplied prompt');
    await ui.getByRole('button', { name: 'Undo', exact: true }).click();
    expect((await snapshot(page)).source).toBe(before.source);
  });
}

test('keep local preserves rejected fields and newest candidate is used', async ({ page }) => {
  const ui = await open(page); await step(page);
  const id = ui.getByLabel('Step ID', { exact: true });
  await id.fill('bad id'); await id.press('Tab');
  await page.evaluate(source => (window as any).first.update({ source }), incoming);
  await ui.getByRole('button', { name: 'Keep local', exact: true }).click();
  await expect(id).toHaveValue('bad id');
  await expect(id).toHaveAttribute('aria-invalid', 'true');
  await page.evaluate(source => {
    (window as any).first.update({ source: source + '\n# newer' });
    (window as any).first.update({ source: source + '\n# newest' });
  }, incoming);
  await ui.getByRole('button', { name: 'Use incoming', exact: true }).click();
  expect((await snapshot(page)).source).toContain('# newest');
  expect((await snapshot(page)).canUndo).toBe(true);
});

test('controlled React echoes protect edits without opening a notice', async ({ page }) => {
  await page.goto('/test/browser.html?controlled');
  await field(page, 'Name', 'Controlled draft');
  await page.evaluate(() => (window as any).changePolicy());
  await expect(page.locator('#a').getByLabel('Name', { exact: true })).toHaveValue('Controlled draft');
  await expect(page.getByRole('region', { name: 'Incoming source replacement' })).toHaveCount(0);
  await expect.poll(() => page.evaluate(() => (window as any).latest.hasLocalChanges)).toBe(true);
});

test('React host replacement prompts and unchanged props do not reopen it', async ({ page }) => {
  await page.goto('/test/browser.html?uncontrolled');
  await field(page, 'Name', 'React draft');
  await page.evaluate(source => (window as any).replaceHostSource(source), incoming);
  await page.getByRole('button', { name: 'Keep local', exact: true }).click();
  await page.evaluate(() => (window as any).rerenderHost());
  await expect(page.getByRole('region', { name: 'Incoming source replacement' })).toHaveCount(0);
  await expect(page.locator('#a').getByLabel('Name', { exact: true })).toHaveValue('React draft');
});

test('clean editors and read-only viewers update immediately', async ({ page }) => {
  for (const query of ['', '?viewer']) {
    await page.goto('/test/browser.html' + query);
    await page.evaluate(source => (window as any).first.update({ source }), incoming);
    await expect(page.locator('#a').getByRole('button', { name: 'Inspect job replacement', exact: true })).toBeVisible();
    await expect(page.getByRole('region', { name: 'Incoming source replacement' })).toHaveCount(0);
    expect((await snapshot(page)).hasLocalChanges).toBe(false);
  }
});
