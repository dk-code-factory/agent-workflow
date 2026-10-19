import { expect, test } from '@playwright/test';

test('resize fits graph, drawers isolate hidden content, short containers keep YAML usable', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => { const element = document.querySelector<HTMLElement>('#a')!; element.style.width = '390px'; element.style.height = '500px'; });
  await expect(first.getByRole('button', { name: 'Visual', exact: true })).toHaveAttribute('aria-pressed', 'true');
  const flow = first.locator('.aw-flow'); const card = first.getByRole('article', { name: 'Job build', exact: true });
  await expect.poll(async () => { const bounds = await flow.boundingBox(); const node = await card.boundingBox(); return !!bounds && !!node && node.x >= bounds.x - 2 && node.x + node.width <= bounds.x + bounds.width + 2; }).toBe(true);
  await first.getByRole('button', { name: 'inspector', exact: true }).click();
  await expect(first.getByRole('heading', { name: 'Workflow', exact: true })).toBeFocused();
  expect(await first.locator('.aw-main').evaluate(element => (element as HTMLElement).inert)).toBe(true);
  await first.getByRole('button', { name: 'canvas', exact: true }).click();
  await first.getByRole('button', { name: 'Split', exact: true }).click();
  const editor = await first.locator('.aw-editor').boundingBox(); const yaml = await first.locator('.aw-source').boundingBox();
  expect(yaml!.height).toBeGreaterThan(50); expect(yaml!.y + yaml!.height).toBeLessThanOrEqual(editor!.y + editor!.height);
});
