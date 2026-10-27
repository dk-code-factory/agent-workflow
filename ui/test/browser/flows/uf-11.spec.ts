import { expect, test } from '@playwright/test';
import { open, snapshot, job } from './harness';

test('remove dependency through its checkbox', async ({ page }) => {
  const ui = await open(page); await job(page, 'test');
  await ui.getByRole('checkbox', { name: 'build', exact: true }).uncheck();
  expect((await snapshot(page)).workflow.jobs.test.needs ?? []).toEqual([]);
  await expect(ui.locator('.react-flow__edge')).toHaveCount(0);
});
