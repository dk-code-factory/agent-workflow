import { expect, test } from '@playwright/test';
import { open, snapshot, field, step } from './harness';

test('review incompatible settings, cancel conversion, and undo the confirmed change', async ({ page }) => {
  const source = 'jobs: {build: {steps: [{id: run, argv: [echo, before], timeout: 2m, working-directory: /work}]}}';
  const ui = await open(page, source);
  await step(page, 'run direct command');
  page.once('dialog', async dialog => {
    expect(dialog.message()).toContain('argv'); expect(dialog.message()).toContain('timeout'); expect(dialog.message()).toContain('working-directory');
    await dialog.dismiss();
  });
  await ui.getByLabel('Step type', { exact: true }).selectOption('agent/claude');
  expect((await snapshot(page)).source).toBe(source);
  await expect(ui.getByLabel('Step type', { exact: true })).toHaveValue('argv');
  page.once('dialog', dialog => dialog.accept());
  await ui.getByLabel('Step type', { exact: true }).selectOption('agent/claude');
  expect((await snapshot(page)).workflow.jobs.build.steps[0]).toEqual({ id: 'run', uses: 'agent/claude', with: { prompt: '' } });
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(source);
});

test('command conversions preserve timeout, directory, and common settings', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {steps: [{id: run, argv: [echo, before], timeout: 2m, working-directory: /work, if: success, continue-on-error: true}]}}');
  await step(page, 'run direct command');
  page.once('dialog', async dialog => { expect(dialog.message()).not.toContain('timeout'); await dialog.accept(); });
  await ui.getByLabel('Step type', { exact: true }).selectOption('shell');
  expect((await snapshot(page)).workflow.jobs.build.steps[0]).toEqual({ id: 'run', shell: 'sh', run: 'echo Hello', timeout: '2m', 'working-directory': '/work', if: 'success', 'continue-on-error': true });
  page.once('dialog', dialog => dialog.accept());
  await ui.getByLabel('Step type', { exact: true }).selectOption('argv');
  expect((await snapshot(page)).workflow.jobs.build.steps[0]).toMatchObject({ argv: ['echo', 'Hello'], timeout: '2m', 'working-directory': '/work' });
});

test('action target and step type use the same reviewed conversion', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {steps: [{id: run, uses: git/commit, with: {path: /work, message: Keep}}]}}');
  await step(page, 'run git/commit');
  page.once('dialog', async dialog => { expect(dialog.message()).toContain('with.message'); expect(dialog.message()).not.toContain('with.path'); await dialog.dismiss(); });
  await field(page, 'Action target', 'workspace/integrate');
  await expect(ui.getByLabel('Action target', { exact: true })).toHaveAttribute('aria-invalid', 'true');
  expect((await snapshot(page)).workflow.jobs.build.steps[0].uses).toBe('git/commit');
  await ui.getByLabel('Action target', { exact: true }).press('Escape');
  page.once('dialog', dialog => dialog.accept());
  await field(page, 'Action target', 'workspace/integrate');
  expect((await snapshot(page)).workflow.jobs.build.steps[0]).toEqual({ id: 'run', uses: 'workspace/integrate', with: { path: '/work' } });
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with).toEqual({ path: '/work', message: 'Keep' });
});

for (const initialMessage of ['Keep', '']) {
  test(`keyboard save retries a cancelled action conversion with an active message draft (${initialMessage || 'initially empty'})`, async ({ page }) => {
    const target = initialMessage ? 'workspace/integrate' : 'agent/claude';
    const source = `jobs: {build: {steps: [{id: run, uses: git/commit, with: {path: /work, message: "${initialMessage}"}}]}}`;
    const ui = await open(page, source);
    await page.evaluate(() => {
      (window as any).saves = [];
      (window as any).first.update({ onSave: (snapshot: any) => { (window as any).saves.push(snapshot); } });
    });
    await step(page, 'run git/commit');
    page.once('dialog', dialog => dialog.dismiss());
    await field(page, 'Action target', target);
    await expect(ui.getByLabel('Action target', { exact: true })).toHaveAttribute('aria-invalid', 'true');
    expect((await snapshot(page)).source).toBe(source);
    const message = ui.getByLabel('Message', { exact: true });
    await message.fill('Changed before retry');
    await expect(message).toBeFocused();
    let discarded = '';
    page.once('dialog', async dialog => { discarded = dialog.message(); await dialog.accept(); });
    await message.press('ControlOrMeta+s');
    await expect.poll(() => page.evaluate(() => (window as any).saves.length)).toBe(1);
    expect(discarded).toContain('with.message');
    const expected = { id: 'run', uses: target, with: initialMessage ? { path: '/work' } : { prompt: '' } };
    expect(await page.evaluate(() => (window as any).saves[0].workflow.jobs.build.steps[0])).toEqual(expected);
    expect((await snapshot(page)).workflow.jobs.build.steps[0]).toEqual(expected);
    await expect(ui.getByRole('status', { name: 'Save status', exact: true })).toHaveText('Saved');
  });
}
