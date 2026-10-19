import { expect, test } from '@playwright/test';
import { open, snapshot, field, job } from './harness';

test('author parallel reviews followed by a combined result', async ({ page }) => {
  const ui = await open(page, 'jobs: {}');
  for (const id of ['review_a', 'review_b', 'summarize']) {
    await ui.getByRole('button', { name: 'Add job', exact: true }).click();
    await field(page, 'Job ID', id);
    await ui.getByRole('button', { name: 'Agent', exact: true }).click();
    await field(page, 'Prompt', id === 'summarize' ? 'Combine reviews' : 'Review changes');
  }
  await job(page, 'summarize');
  for (const name of ['review_a', 'review_b']) await ui.getByRole('checkbox', { name, exact: true }).check();
  await ui.getByRole('button', { name: 'Auto layout', exact: true }).click();
  const jobs = (await snapshot(page)).workflow.jobs;
  expect(jobs.summarize.needs).toEqual(['review_a', 'review_b']);
  expect(jobs.review_a.needs).toBeUndefined(); expect(jobs.review_b.needs).toBeUndefined();
  await expect(ui.locator('.react-flow__edge')).toHaveCount(2);
});
