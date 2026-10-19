import { expect, test } from '@playwright/test';
import { open, snapshot, job, step } from './harness';

test('duplicate jobs and steps with unique identities and undo', async ({ page }) => {
  const ui = await open(page); await job(page);
  await ui.getByRole('button', { name: 'Duplicate job', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build_copy.steps[0].id).toBe('greet');
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build_copy).toBeUndefined();
  await step(page); await ui.getByRole('button', { name: 'Duplicate step', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps.map((s: any) => s.id)).toEqual(['greet', 'greet_copy']);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps).toHaveLength(1);
});
