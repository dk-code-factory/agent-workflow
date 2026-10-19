import { expect, test } from '@playwright/test';

test('canvas connections create dependencies and reject cycles', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({
    source: 'jobs: {build: {steps: [{argv: [echo]}]}, test: {steps: [{argv: [echo]}]}}',
    onLayoutChange: (layout: any) => { (window as any).savedLayout = layout; },
  }));
  await first.getByRole('button', { name: 'Visual', exact: true }).click();
  await first.getByRole('button', { name: 'Fit view', exact: true }).click();
  const node = (id: string) => first.locator(`.react-flow__node[data-id="${id}"]`);
  const connect = async (from: string, to: string) => {
    await node(from).locator('.source').dragTo(node(to).locator('.target'));
  };
  await connect('build', 'test');
  await expect.poll(() => page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.test.needs)).toEqual(['build']);
  await connect('test', 'build'); await expect(first.getByRole('alert')).toContainText('cycle');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.needs)).toBeUndefined();
});
