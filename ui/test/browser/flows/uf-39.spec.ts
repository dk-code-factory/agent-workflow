import { expect, test } from '@playwright/test';
import { open, snapshot, field, job } from './harness';

test('declare producer output and insert it in dependent prompt', async ({ page }) => {
  const ui = await open(page, 'jobs: {review: {steps: [{id: analyze, uses: agent/claude, with: {prompt: Review}}]}, summarize: {steps: [{id: summary, uses: agent/claude, with: {prompt: Summarize}}]}}');
  await job(page, 'review'); await ui.getByText('Job outputs', { exact: true }).click();
  await ui.getByRole('button', { name: 'Add output', exact: true }).click();
  await field(page, 'Identifier: output', 'report'); await field(page, 'Output value: report', '${{ steps.analyze.outputs.text }}');
  await job(page, 'summarize'); await ui.getByRole('checkbox', { name: 'review', exact: true }).check();
  await ui.getByRole('button', { name: '1. summary', exact: true }).click();
  await ui.getByLabel('Insert expression into Prompt', { exact: true }).selectOption('needs.review.outputs.report');
  const jobs = (await snapshot(page)).workflow.jobs;
  expect(jobs.review.outputs.report).toBe('${{ steps.analyze.outputs.text }}');
  expect(jobs.summarize.needs).toEqual(['review']);
  expect(jobs.summarize.steps[0].with.prompt).toContain('${{ needs.review.outputs.report }}');
});

test('preview and cancel result wiring, then apply and Undo the complete transaction', async ({ page }) => {
  const ui = await open(page, 'jobs: {review: {steps: [{id: analyze, uses: agent/claude, with: {prompt: Review}}]}, summarize: {steps: [{id: summary, uses: agent/claude, with: {prompt: "Summarize: ", model: keep}}]}}');
  await job(page, 'summarize');
  await ui.getByRole('button', { name: '1. summary', exact: true }).click();
  const before = (await snapshot(page)).source;
  const picker = ui.getByRole('dialog', { name: 'Use job result', exact: true });
  await ui.getByRole('button', { name: 'Use job result in Prompt', exact: true }).click();
  await picker.getByLabel('Job output name', { exact: true }).fill('report');
  await picker.getByRole('button', { name: 'Preview result wiring', exact: true }).click();
  await expect(picker.getByLabel('Result wiring preview', { exact: true })).toContainText('Declare review.outputs.report: ${{ steps.analyze.outputs.text }}');
  await expect(picker.getByLabel('Result wiring preview', { exact: true })).toContainText('Add dependency: summarize needs review');
  expect((await snapshot(page)).source).toBe(before);
  await picker.getByRole('button', { name: 'Cancel', exact: true }).click();
  expect((await snapshot(page)).source).toBe(before);
  await ui.getByRole('button', { name: 'Use job result in Prompt', exact: true }).focus();
  await page.keyboard.press('Enter');
  await picker.getByLabel('Job output name', { exact: true }).fill('report');
  await picker.getByRole('button', { name: 'Preview result wiring', exact: true }).click();
  await picker.getByRole('button', { name: 'Apply job result', exact: true }).click();
  const jobs = (await snapshot(page)).workflow.jobs;
  expect(jobs.review.outputs).toEqual({ report: '${{ steps.analyze.outputs.text }}' });
  expect(jobs.summarize.needs).toEqual(['review']);
  expect(jobs.summarize.steps[0].with).toEqual({ prompt: 'Summarize: ${{ needs.review.outputs.report }}', model: 'keep' });
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(before);
});

test('existing outputs are reusable and changed documents require a new preview', async ({ page }) => {
  const ui = await open(page, 'jobs: {review: {outputs: {report: "${{ steps.analyze.outputs.text }}"}, steps: [{id: analyze, argv: [echo]}]}, summarize: {steps: [{uses: agent/claude, with: {prompt: Summary}}]}}');
  await job(page, 'summarize'); await ui.getByRole('button', { name: '1. step_1', exact: true }).click();
  await ui.getByRole('button', { name: 'Use job result in Prompt', exact: true }).click();
  const picker = ui.getByRole('dialog');
  await picker.getByLabel('Result source', { exact: true }).selectOption('output');
  await picker.getByLabel('Producer output', { exact: true }).selectOption('report');
  await picker.getByRole('button', { name: 'Preview result wiring', exact: true }).click();
  await expect(picker.getByLabel('Result wiring preview', { exact: true })).toContainText('Reuse review.outputs.report');
  await page.evaluate(() => { const editor = (window as any).first; editor.update({ source: '# host update\n' + editor.getSnapshot().source }); });
  const changed = (await snapshot(page)).source;
  await picker.getByRole('button', { name: 'Apply job result', exact: true }).click();
  await expect(picker.getByRole('alert')).toContainText('document changed');
  expect((await snapshot(page)).source).toBe(changed);
  await picker.getByRole('button', { name: 'Preview result wiring', exact: true }).click();
  await picker.getByRole('button', { name: 'Apply job result', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.review.outputs).toEqual({ report: '${{ steps.analyze.outputs.text }}' });
});
