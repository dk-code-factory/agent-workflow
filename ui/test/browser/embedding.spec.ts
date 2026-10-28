import { expect, test } from '@playwright/test';

test('plain API, keyboard forms, shared undo, custom cleanup and independent instances', async ({ page }) => {
  const errors: string[] = []; page.on('pageerror', e => errors.push(e.message));
  await page.goto('/test/browser.html');
  const first = page.locator('#a'); const second = page.locator('#b');
  await expect(first.getByRole('button', { name: 'Inspect job build', exact: true })).toBeVisible();
  await first.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await first.getByLabel('Job ID', { exact: true }).fill('compile'); await page.keyboard.press('Enter');
  await expect(first.getByRole('button', { name: 'Inspect job compile', exact: true })).toBeVisible();
  await expect(second.getByRole('button', { name: 'Inspect job independent', exact: true })).toBeVisible();
  await first.getByRole('button', { name: 'Undo', exact: true }).click();
  await expect(first.getByRole('button', { name: 'Inspect job build', exact: true })).toBeVisible();
  await first.getByRole('button', { name: 'greet', exact: false }).first().click();
  await first.getByRole('button', { name: 'Custom prompt', exact: true }).click();
  await expect(first.getByLabel('Prompt', { exact: true })).toHaveValue('Custom value');
  await expect.poll(() => page.evaluate(() => (window as any).cleanupCount)).toBeGreaterThan(0);
  await first.getByRole('button', { name: 'Undo', exact: true }).click();
  await expect(first.getByLabel('Prompt', { exact: true })).toHaveValue('Hello');
  await first.getByRole('button', { name: 'YAML', exact: true }).click();
  const yaml = first.getByRole('textbox', { name: 'Workflow YAML' });
  await yaml.fill('jobs: [broken');
  await expect(first.getByText('Stale preview', { exact: false })).toBeVisible();
  await yaml.press('ControlOrMeta+z');
  await expect(first.getByText('Stale preview', { exact: false })).toHaveCount(0);
  await page.evaluate(() => (window as any).first.update({ theme: 'dark' }));
  await expect(first.locator('.aw-editor')).toHaveAttribute('data-theme', 'dark');
  await page.evaluate(() => (window as any).first.destroy()); await expect(first.locator('.aw-editor')).toHaveCount(0);
  await expect(second.locator('.aw-editor')).toBeVisible(); expect(errors).toEqual([]);
});

test('React strict mode, custom forms and keyboard dependency picker', async ({ page }) => {
  const errors: string[] = []; page.on('pageerror', e => errors.push(e.message));
  await page.goto('/test/browser.html?react'); const first = page.locator('#a');
  await first.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  const dep = first.getByRole('checkbox', { name: 'test', exact: true }); await dep.focus(); await page.keyboard.press('Space');
  await expect(first.getByRole('alert')).toContainText('cycle');
  await first.getByRole('button', { name: 'greet', exact: false }).first().click();
  await first.getByLabel('Custom model', { exact: true }).fill('host-model'); await page.keyboard.press('Tab');
  await expect.poll(() => page.evaluate(() => (window as any).latest?.source)).toContain('host-model');
  await first.getByRole('button', { name: 'Custom prompt', exact: true }).click();
  await expect(first.getByLabel('Prompt', { exact: true })).toHaveValue('Custom value');
  await page.evaluate(() => (window as any).reactUnmount()); expect(errors).toEqual([]);
});

test('container resizing, search, themes and layout stay outside source', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  const before = await page.evaluate(() => (window as any).first.getSnapshot().source);
  await first.getByRole('button', { name: 'Auto layout', exact: true }).click();
  await first.getByRole('button', { name: 'Fit view', exact: true }).click();
  await first.getByRole('textbox', { name: 'Search workflow', exact: true }).fill('greet');
  await expect(first.locator('.aw-dimmed')).toHaveCount(1);
  await page.evaluate(() => { document.querySelector<HTMLElement>('#a')!.style.width = '500px'; (window as any).first.update({ theme: 'light', tokens: { '--aw-accent': '#123456' } }); });
  await expect(first.getByRole('navigation', { name: 'Panels' })).toBeVisible();
  await first.getByRole('button', { name: 'inspector', exact: true }).click(); await expect(first.getByLabel('Name', { exact: true })).toBeVisible();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().source)).toBe(before);
  await expect(first.locator('.aw-editor')).toHaveCSS('--aw-accent', '#123456');
});

test('React viewer guards custom mutations and unmount cleans up renderers', async ({ page }) => {
  await page.goto('/test/browser.html?react&viewer'); const first = page.locator('#a');
  await first.getByRole('button', { name: 'greet', exact: false }).first().click();
  await expect(first.getByRole('button', { name: 'Custom prompt', exact: true })).toBeDisabled();
  await expect(first.getByLabel('Custom model', { exact: true })).toHaveAttribute('readonly', '');
  const before = await page.evaluate(() => (window as any).cleanupCount);
  await page.evaluate(() => (window as any).reactUnmount());
  expect(await page.evaluate(() => (window as any).cleanupCount)).toBeGreaterThan(before);
});

test('valid identifiers matching object property names render and remain editable', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: 'jobs: {constructor: {steps: [{argv: [echo]}]}, __proto__: {steps: [{argv: [echo]}]}}' }));
  await first.getByRole('button', { name: 'Fit view', exact: true }).click();
  await expect(first.getByRole('button', { name: 'Inspect job constructor', exact: true })).toBeVisible();
  await expect(first.getByRole('button', { name: 'Inspect job __proto__', exact: true })).toBeVisible();
  await expect(first.getByRole('button', { name: 'Expand constructor', exact: true })).toHaveAttribute('aria-expanded', 'true');
  await first.getByRole('button', { name: 'Inspect job constructor', exact: true }).click();
  await first.getByLabel('Job ID', { exact: true }).fill('toString'); await page.keyboard.press('Enter');
  await expect(first.getByRole('button', { name: 'Inspect job toString', exact: true })).toBeVisible();
});


test('uncontrolled React host rerenders preserve local drafts and accept source replacements', async ({ page }) => {
  const errors: string[] = []; page.on('pageerror', error => errors.push(error.message));
  await page.goto('/test/browser.html?uncontrolled'); const first = page.locator('#a');
  await first.getByLabel('Name', { exact: true }).fill('Local draft');
  await page.keyboard.press('Tab');
  await expect.poll(() => page.evaluate(() => (window as any).latest.workflow.name)).toBe('Local draft');
  await page.evaluate(() => (window as any).rerenderHost());
  await expect(first.locator('.aw-editor')).toHaveAttribute('data-theme', 'dark');
  await expect(first.getByLabel('Name', { exact: true })).toHaveValue('Local draft');
  await expect.poll(() => page.evaluate(() => (window as any).latest.workflow.name)).toBe('Local draft');
  await page.evaluate(() => (window as any).replaceHostSource('name: Host replacement\njobs: {replacement: {steps: [{argv: [echo]}]}}'));
  await expect(first.getByLabel('Name', { exact: true })).toHaveValue('Local draft');
  await first.getByRole('button', { name: 'Use incoming', exact: true }).click();
  await expect(first.getByLabel('Name', { exact: true })).toHaveValue('Host replacement');
  await first.getByRole('button', { name: 'Undo', exact: true }).click();
  await expect(first.getByLabel('Name', { exact: true })).toHaveValue('Local draft');
  expect(errors).toEqual([]);
});

test('custom renderer callbacks stop mutating after replacement or teardown', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await first.getByRole('button', { name: 'greet', exact: false }).first().click();
  await expect(first.getByRole('button', { name: 'Custom prompt', exact: true })).toBeVisible();
  await page.evaluate(() => { (window as any).oldCustomSetField = (window as any).customSetField; });
  await first.getByRole('button', { name: 'Custom prompt', exact: true }).click();
  await expect(first.getByLabel('Prompt', { exact: true })).toHaveValue('Custom value');
  await page.evaluate(() => (window as any).oldCustomSetField(['with', 'prompt'], 'Obsolete callback'));
  await expect(first.getByLabel('Prompt', { exact: true })).toHaveValue('Custom value');
  await page.evaluate(() => { (window as any).first.destroy(); (window as any).customSetField(['with', 'prompt'], 'After teardown'); });
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.steps[0].with.prompt)).toBe('Custom value');
});

test('imperative destruction silences source echoes before cleanup and aborts validation', async ({ page }) => {
  const errors: string[] = []; page.on('pageerror', error => errors.push(error.message));
  await page.goto('/test/browser.html'); const ui = page.locator('#a');
  await ui.getByRole('button', { name: 'greet', exact: false }).first().click();
  await page.evaluate(() => {
    const host = (window as any); host.destroyNotifications = 0; host.destroying = false; host.oldDestroyField = host.customSetField;
    host.first.update({ onChange: (snapshot: any) => {
      if (host.destroying) host.destroyNotifications++;
      host.first.update({ source: snapshot.source });
    }, validate: (_snapshot: any, signal: AbortSignal) => new Promise(() => { host.destroySignal = signal; }) });
  });
  await ui.getByLabel('Prompt', { exact: true }).fill('Unapplied teardown draft');
  expect(await page.evaluate(() => (window as any).destroySignal.aborted)).toBe(false);
  await page.evaluate(() => {
    const host = (window as any); host.destroyBefore = host.first.getSnapshot().source; host.destroying = true;
    host.first.destroy(); host.first.destroy(); host.oldDestroyField(['with', 'prompt'], 'Obsolete cleanup write');
  });
  await expect(ui.locator('.aw-editor')).toHaveCount(0);
  expect(await page.evaluate(() => ({ notifications: (window as any).destroyNotifications, aborted: (window as any).destroySignal.aborted, source: (window as any).first.getSnapshot().source, original: (window as any).destroyBefore }))).toMatchObject({ notifications: 0, aborted: true });
  expect(await page.evaluate(() => (window as any).first.getSnapshot().source === (window as any).destroyBefore)).toBe(true);
  expect(errors).toEqual([]);
});
