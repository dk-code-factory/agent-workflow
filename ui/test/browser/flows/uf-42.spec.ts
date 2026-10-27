import { expect, test } from '@playwright/test';
import { open, snapshot, job } from './harness';

test('replace reusable call with starter steps and undo', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {uses: workflows/review.yml, with: {task: Review}, secrets: {token: secret}, outputs: {report: result}}}');
  await job(page); const before = (await snapshot(page)).source;
  page.once('dialog', async dialog => {
    for (const field of ['uses', 'with', 'secrets']) expect(dialog.message()).toContain(field);
    await dialog.dismiss();
  });
  await ui.getByLabel('Job kind', { exact: true }).selectOption('steps');
  expect((await snapshot(page)).source).toBe(before);
  page.once('dialog', dialog => dialog.accept());
  await ui.getByLabel('Job kind', { exact: true }).selectOption('steps');
  const build = (await snapshot(page)).workflow.jobs.build;
  expect(build.uses).toBeUndefined(); expect(build.with).toBeUndefined(); expect(build.secrets).toBeUndefined();
  expect(build.steps).toEqual([{ id: 'step_1', argv: ['echo', 'Hello'] }]);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(before);
  expect((await snapshot(page)).workflow.jobs.build).toMatchObject({ uses: 'workflows/review.yml', with: { task: 'Review' }, secrets: { token: 'secret' } });
});

test('imported reusable job retains existing local steps when converted and undo restores source', async ({ page }) => {
  const source = `# Imported mixed job
jobs:
  build:
    uses: workflows/review.yml
    with: {task: Review}
    secrets: {token: secret}
    outputs: {report: result}
    steps:
      # Keep the configured command
      - id: configured
        argv: [echo, retained]
        timeout: 2m
`;
  const ui = await open(page, source);
  await job(page);
  const originalSteps = (await snapshot(page)).workflow.jobs.build.steps;
  const review = (message: string) => {
    for (const field of ['uses', 'with', 'secrets']) expect(message).toContain(`• ${field}`);
    expect(message).not.toContain('• steps');
  };
  page.once('dialog', async dialog => { review(dialog.message()); await dialog.dismiss(); });
  await ui.getByLabel('Job kind', { exact: true }).selectOption('steps');
  expect((await snapshot(page)).source).toBe(source);
  page.once('dialog', async dialog => { review(dialog.message()); await dialog.accept(); });
  await ui.getByLabel('Job kind', { exact: true }).selectOption('steps');
  const converted = await snapshot(page);
  expect(converted.workflow.jobs.build).toEqual({ outputs: { report: 'result' }, steps: originalSteps });
  expect(converted.source).toContain('# Keep the configured command');
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(source);
});
