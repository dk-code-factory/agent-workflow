import { expect, type Page } from '@playwright/test';

// Host operations only set up fixtures or inspect results; authoring uses controls.
export async function open(page: Page, source?: string) {
  await page.goto('/test/browser.html');
  if (source !== undefined) await page.evaluate(source => (window as any).first.update({ source }), source);
  return page.locator('#a');
}
export const snapshot = (page: Page): Promise<any> => page.evaluate(() => (window as any).first.getSnapshot());
export async function field(page: Page, label: string, value: string) {
  const control = page.locator('#a').getByLabel(label, { exact: true });
  await control.fill(value); await control.press('Tab');
}
export async function job(page: Page, id = 'build') {
  await page.locator('#a').getByRole('button', { name: `Inspect job ${id}`, exact: true }).click();
}
export async function step(page: Page, name = 'greet') {
  await page.locator('#a').getByRole('button', { name, exact: false }).first().click();
}
export async function downloadText(page: Page, button = 'Export YAML') {
  const [download] = await Promise.all([page.waitForEvent('download'), page.getByRole('button', { name: button, exact: true }).first().click()]);
  const { readFile } = await import('node:fs/promises');
  expect(download.suggestedFilename()).toMatch(/\.yml$/);
  return readFile((await download.path())!, 'utf8');
}
