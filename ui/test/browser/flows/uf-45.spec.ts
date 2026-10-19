import { expect, test } from '@playwright/test';

test('deletion traps keyboard focus and Escape restores its origin', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await first.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  const remove = first.getByRole('button', { name: 'Delete job', exact: true }); await remove.click();
  await expect(first.getByRole('button', { name: 'Cancel', exact: true })).toBeFocused();
  await page.keyboard.press('Shift+Tab'); await expect(first.getByRole('button', { name: 'Confirm delete', exact: true })).toBeFocused();
  await page.keyboard.press('Shift+Tab'); await expect(first.getByRole('button', { name: 'Cancel', exact: true })).toBeFocused();
  await page.keyboard.press('Tab'); await expect(first.getByRole('button', { name: 'Confirm delete', exact: true })).toBeFocused();
  await page.keyboard.press('Tab'); await expect(first.getByRole('button', { name: 'Cancel', exact: true })).toBeFocused();
  await page.keyboard.press('Escape'); await expect(remove).toBeFocused();
});


test('keyboard navigation edits a name and authors a dependency', async ({ page }) => {
  await page.goto('/test/browser.html'); const ui = page.locator('#a');
  const build = ui.locator('.aw-palette').getByRole('button', { name: 'build', exact: true });
  await build.focus(); await page.keyboard.press('Enter');
  await expect(ui.getByLabel('Job ID', { exact: true })).toHaveValue('build');
  // The next palette entry is reachable with Tab and can be activated with Space.
  await build.focus(); await page.keyboard.press('Tab');
  await expect(ui.locator('.aw-palette').getByRole('button', { name: 'test', exact: true })).toBeFocused();
  await page.keyboard.press('Space');
  const name = ui.getByLabel('Name', { exact: true }); await name.focus();
  await page.keyboard.type('Keyboard consumer'); await page.keyboard.press('Enter');
  const dependency = ui.getByRole('checkbox', { name: 'build', exact: true });
  await dependency.focus(); await page.keyboard.press('Space'); await expect(dependency).not.toBeChecked();
  await page.keyboard.press('Space'); await expect(dependency).toBeChecked();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.test)).toMatchObject({ name: 'Keyboard consumer', needs: ['build'] });
});
