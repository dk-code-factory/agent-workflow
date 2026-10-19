import { expect, test } from '@playwright/test';
import { open, snapshot } from './harness';

test('collapse zoom and rearrange without editing source', async ({ page }) => {
  const ui = await open(page); const before = (await snapshot(page)).source;
  const expand = ui.getByRole('button', { name: 'Expand build', exact: true });
  await expand.click(); await expect(expand).toHaveAttribute('aria-expanded', 'false');
  await expand.click(); await expect(expand).toHaveAttribute('aria-expanded', 'true');
  await ui.getByRole('button', { name: 'Zoom In', exact: true }).click();
  await ui.getByRole('button', { name: 'Fit view', exact: true }).click();
  await ui.getByRole('button', { name: 'Auto layout', exact: true }).click();
  expect((await snapshot(page)).source).toBe(before);
});


test('dragging persists card positions without changing YAML', async ({ page }) => {
  const first = await open(page);
  await page.evaluate(() => (window as any).first.update({ onLayoutChange: (layout: any) => { (window as any).savedLayout = layout; } }));
  await first.getByRole('button', { name: 'Visual', exact: true }).click();
  await first.getByRole('button', { name: 'Fit view', exact: true }).click();
  const node = (id: string) => first.locator(`.react-flow__node[data-id="${id}"]`);
  const source = await page.evaluate(() => (window as any).first.getSnapshot().source);
  const before = await node('build').evaluate(element => (element as HTMLElement).style.transform);
  const header = node('build').locator('.aw-card-title'); await header.hover({ position: { x: 15, y: 5 } });
  const box = await header.boundingBox();
  await page.mouse.move(box!.x + 15, box!.y + 5); await page.mouse.down(); await page.mouse.move(box!.x + 75, box!.y + 35, { steps: 12 }); await page.mouse.up();
  await expect.poll(() => node('build').evaluate(element => (element as HTMLElement).style.transform)).not.toBe(before);
  expect(await page.evaluate(() => (window as any).savedLayout.positions.build)).toBeTruthy();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().source)).toBe(source);
});
