import { expect, test } from '@playwright/test';
import { open, snapshot, field } from './harness';

test('add a unique job and configure its properties', async ({ page }) => {
  const ui = await open(page);
  await ui.getByRole('button', { name: 'Add job', exact: true }).click();
  await field(page, 'Job ID', 'review'); await field(page, 'Name', 'Review changes'); await field(page, 'Runner', 'linux');
  expect((await snapshot(page)).workflow.jobs.review).toMatchObject({ name: 'Review changes', 'runs-on': 'linux', steps: [{ argv: ['echo', 'Hello'] }] });
  await ui.getByRole('button', { name: 'Add job', exact: true }).click();
  expect(Object.keys((await snapshot(page)).workflow.jobs)).toEqual(['build', 'test', 'review', 'job']);
});
