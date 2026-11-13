import { expect, test } from '@playwright/test';
import { open, snapshot, field, job, step } from './harness';

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

test('edit a local parallel block and consume joined nested results', async ({ page }) => {
  const ui = await open(page, `jobs:
  review:
    steps:
      - id: reviews
        parallel:
          - id: first
            uses: agent/run
            with:
              agent: claude.toml
              prompt: First review
          - sequence:
              - id: collect
                argv: [ echo, collect ]
              - id: second
                uses: agent/run
                with:
                  agent: claude.toml
                  prompt: \${{ steps.collect.outputs.stdout }}
      - id: combine
        uses: agent/run
        with:
          agent: claude.toml
          prompt: Combine reviews
`);
  const before = (await snapshot(page)).source;
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await step(page, 'reviews parallel block');
  await expect(ui.getByLabel('Step type', { exact: true })).toHaveValue('parallel');
  await expect(ui.getByRole('checkbox', { name: 'Continue on error', exact: true })).toHaveCount(0);
  const branches = ui.getByLabel('Parallel branches (YAML)', { exact: true });
  await expect(branches).toHaveValue(/sequence:/);
  await field(page, 'Parallel branches (YAML)', `- id: first
  uses: agent/run
  with:
    agent: claude.toml
    prompt: Revised first review
- sequence:
    - id: collect
      argv: [ echo, collect ]
    - id: second
      uses: agent/run
      with:
        agent: claude.toml
        prompt: \${{ steps.collect.outputs.stdout }}
`);
  expect((await snapshot(page)).workflow.jobs.review.steps[0].parallel[0].with.prompt).toBe('Revised first review');
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  const edited = (await snapshot(page)).source;
  await ui.getByRole('button', { name: 'Duplicate step', exact: true }).click();
  const copy = (await snapshot(page)).workflow.jobs.review.steps[1];
  expect(copy.parallel[0].id).toBe('first_copy');
  expect(copy.parallel[1].sequence[1].with.prompt).toContain('steps.collect_copy.outputs.stdout');
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(edited);
  page.once('dialog', async dialog => { expect(dialog.message()).toContain('parallel'); await dialog.dismiss(); });
  await ui.getByLabel('Step type', { exact: true }).selectOption('argv');
  expect((await snapshot(page)).source).toBe(edited);
  await step(page, 'combine agent/run');
  await ui.getByLabel('Insert expression into Prompt', { exact: true }).selectOption('steps.second.text');
  expect((await snapshot(page)).workflow.jobs.review.steps[1].with.prompt).toContain('${{ steps.second.text }}');
  await expect(ui.getByLabel('Insert expression into Prompt', { exact: true }).locator('option[value="steps.first.text"]')).toHaveCount(1);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(before);
});

test('add a parallel block from the palette', async ({ page }) => {
  const ui = await open(page, 'jobs: {review: {steps: [{id: collect, argv: [echo]}]}}');
  await job(page, 'review');
  await ui.getByRole('button', { name: 'Parallel block', exact: true }).click();
  await expect(ui.getByLabel('Parallel branches (YAML)', { exact: true })).toBeVisible();
  const parallel = (await snapshot(page)).workflow.jobs.review.steps[1].parallel;
  expect(parallel).toHaveLength(2);
  expect(parallel[0].id).not.toBe(parallel[1].id);
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
});

test('duplicated nested consumers use contracts bound to their copied producers', async ({ page }) => {
  const ui = await open(page, `schemas:
  Shared:
    type: string
    enum: [ '\${{ steps.collect.outputs.stdout }}' ]
jobs:
  review:
    steps:
      - id: reviews
        parallel:
          - sequence:
              - { id: collect, argv: [ echo, collect ] }
              - { id: consume, with: { output: { schema: Shared }, agent: claude.toml, prompt: Review }, uses: agent/run }
          - { id: other, argv: [ echo, other ] }
      - { id: after, with: { output: { schema: Shared }, agent: claude.toml, prompt: Original }, uses: agent/run }
`);
  const before = (await snapshot(page)).source;
  await step(page, 'reviews parallel block');
  await ui.getByRole('button', { name: 'Duplicate step', exact: true }).click();
  const copied = await snapshot(page);
  expect(copied.workflow.jobs.review.steps[1].parallel[0].sequence[1].with.output.schema).toBe('Shared_copy');
  expect(copied.workflow.schemas.Shared_copy.enum).toEqual(['${{ steps.collect_copy.outputs.stdout }}']);
  expect(copied.workflow.schemas.Shared.enum).toEqual(['${{ steps.collect.outputs.stdout }}']);
  expect(copied.workflow.jobs.review.steps[2].with.output.schema).toBe('Shared');
  expect(copied.diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(before);
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(copied.source);
});
