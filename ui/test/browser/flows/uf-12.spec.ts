import { expect, test } from '@playwright/test';
import { open, snapshot, field, job } from './harness';

test('convert a job to a reusable call and restore it with undo', async ({ page }) => {
  const ui = await open(page); await job(page);
  const before = (await snapshot(page)).source;
  page.once('dialog', async dialog => { expect(dialog.message()).toContain('steps'); await dialog.dismiss(); });
  await ui.getByLabel('Job kind', { exact: true }).selectOption('reusable');
  expect((await snapshot(page)).source).toBe(before);
  await expect(ui.getByLabel('Job kind', { exact: true })).toHaveValue('steps');
  page.once('dialog', dialog => dialog.accept());
  await ui.getByLabel('Job kind', { exact: true }).selectOption('reusable');
  await field(page, 'Reusable workflow', 'workflows/review.yml');
  await field(page, 'Call inputs (YAML)', 'task: Review');
  await field(page, 'Call secrets (YAML)', 'token: ${{ secrets.token }}');
  expect((await snapshot(page)).workflow.jobs.build).toMatchObject({ uses: 'workflows/review.yml', with: { task: 'Review' }, secrets: { token: '${{ secrets.token }}' } });
  expect((await snapshot(page)).workflow.jobs.build.steps).toBeUndefined();
  for (let i = 0; i < 4; i++) await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].id).toBe('greet');
});
