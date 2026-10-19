import { expect, test } from '@playwright/test';

test('keyboard save commits the active property draft', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ onSave: (snapshot: any) => { (window as any).savedSource = snapshot.source; return new Promise<void>(resolve => { (window as any).finishSave = resolve; }); } }));
  await first.getByRole('button', { name: 'greet', exact: false }).first().click();
  const prompt = first.getByLabel('Prompt', { exact: true }); await prompt.fill('Save this active draft');
  await prompt.press('ControlOrMeta+s');
  await expect.poll(() => page.evaluate(() => (window as any).savedSource)).toContain('Save this active draft');
  await expect(first.getByRole('button', { name: 'Saving…', exact: true })).toBeDisabled();
  await page.evaluate(() => (window as any).finishSave());
  await expect(first.getByRole('button', { name: 'Save', exact: true })).toBeEnabled();
});


test('failed saving dismisses and can be retried', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ onSave: async () => { throw new Error('Cannot save now'); } }));
  await first.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(first.getByRole('alert')).toContainText('Cannot save now');
  await first.getByRole('button', { name: 'Dismiss', exact: true }).click(); await expect(first.getByRole('alert')).toHaveCount(0);
  await page.evaluate(() => (window as any).first.update({ onSave: (snapshot: any) => { (window as any).savedSource = snapshot.source; } }));
  await first.getByRole('button', { name: 'Save', exact: true }).click();
  await expect.poll(() => page.evaluate(() => (window as any).savedSource)).toContain('Browser workflow');
});
