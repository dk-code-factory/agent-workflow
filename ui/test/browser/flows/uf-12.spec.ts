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
  await expect(ui.getByText('The host supplied no interface metadata', { exact: false })).toBeVisible();
  await ui.getByText('Advanced call YAML', { exact: true }).click();
  await field(page, 'Call inputs (YAML)', 'task: Review');
  await field(page, 'Call secrets (YAML)', 'token: ${{ secrets.token }}');
  expect((await snapshot(page)).workflow.jobs.build).toMatchObject({ uses: 'workflows/review.yml', with: { task: 'Review' }, secrets: { token: '${{ secrets.token }}' } });
  expect((await snapshot(page)).workflow.jobs.build.steps).toBeUndefined();
  for (let i = 0; i < 4; i++) await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].id).toBe('greet');
});

test('named secret edits preserve bindings outside the catalog', async ({ page }) => {
  const ui = await open(page, 'jobs:\n  call:\n    uses: child.yml\n    secrets:\n      token: old\n      extra: keep # preserve secret\n');
  await page.evaluate(() => (window as any).first.update({ workflows: [{ id: 'child.yml', interface: { secrets: { token: { required: true } } } }] }));
  await job(page, 'call');
  await field(page, 'Call secret: token', '${{ secrets.token }}');
  expect((await snapshot(page)).workflow.jobs.call.secrets).toEqual({ token: '${{ secrets.token }}', extra: 'keep' });
  expect((await snapshot(page)).source).toContain('# preserve secret');
});

test('catalog metadata guides typed bindings without losing unknown inputs or inherited secrets', async ({ page }) => {
  const ui = await open(page, 'jobs:\n  call:\n    uses: ./workflows/review.yml\n    with:\n      unknown: [keep] # keep binding\n    secrets: inherit\n');
  await page.evaluate(() => (window as any).first.update({ workflows: [{ id: 'workflows/review.yml', interface: {
    inputs: { task: { type: 'string', required: true }, count: { type: 'number', default: 3 }, flag: { type: 'boolean' }, config: { type: 'object' } },
    secrets: { token: { required: true } },
  } }] }));
  await job(page, 'call');
  await expect(ui.getByText('Default (used when omitted): 3', { exact: true })).toBeVisible();
  expect((await snapshot(page)).workflow.jobs.call.with).toEqual({ unknown: ['keep'] });
  await field(page, 'Call input: task', 'Review "this task"');
  await field(page, 'Call input: count', '0');
  await ui.getByLabel('Call input: flag', { exact: true }).selectOption('false');
  await field(page, 'Call input: config (YAML)', 'items: [one, two]');
  expect((await snapshot(page)).workflow.jobs.call).toMatchObject({ secrets: 'inherit', with: { unknown: ['keep'], task: 'Review "this task"', count: 0, flag: false, config: { items: ['one', 'two'] } } });
  expect((await snapshot(page)).source).toContain('# keep binding');
  await ui.getByLabel('Call input: count editor', { exact: true }).selectOption('expression');
  await field(page, 'Call input: count', '${{ inputs.count }}');
  expect((await snapshot(page)).workflow.jobs.call.with.count).toBe('${{ inputs.count }}');
  await ui.getByRole('button', { name: 'Configure named secrets', exact: true }).click();
  await field(page, 'Call secret: token', '${{ secrets.token }}');
  expect((await snapshot(page)).workflow.jobs.call.secrets).toEqual({ token: '${{ secrets.token }}' });
});
