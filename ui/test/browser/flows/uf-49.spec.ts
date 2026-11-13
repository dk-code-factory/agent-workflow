import { expect, test } from '@playwright/test';
import { open, snapshot, field, job, downloadText } from './harness';
import { parse } from 'yaml';

test('configure a structured helper with named and inline schemas and ordinary-command recovery', async ({ page }) => {
  const source = `schemas: {Output: {type: object, properties: {head: {type: string}}}}
jobs: {build: {container: ubuntu:24.04, steps: [{id: helper, argv: [helper]}, {id: read, argv: [echo, ready]}]}}`;
  const ui = await open(page, source); await job(page);
  await ui.getByRole('button', { name: '1. helper', exact: true }).click();
  await ui.getByText('Helper protocol and environment', { exact: true }).click();
  await ui.getByLabel('Protocol', { exact: true }).selectOption('action-v1');
  await ui.getByLabel('Effect', { exact: true }).selectOption('read_only');
  await field(page, 'Input schema (YAML)', '{type: object}');
  await field(page, 'Output schema (YAML)', 'Output');
  const input = { repository: '.', options: { branches: ['main', 'release'], flags: { dryRun: true }, count: 2 } };
  await field(page, 'Business input (YAML)', '{repository: ".", options: {branches: [main, release], flags: {dryRun: true}, count: 2}}');
  await field(page, 'Environment (YAML)', '{TOKEN: "${{ secrets.token }}"}');
  const helper = (await snapshot(page)).workflow.jobs.build.steps[0];
  expect(helper).toMatchObject({ protocol: 'action-v1', effect: 'read_only', 'input-schema': { type: 'object' }, 'output-schema': 'Output', input, env: { TOKEN: '${{ secrets.token }}' } });
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  const exported = await downloadText(page); expect(exported).toContain('output-schema: Output');
  expect(parse(exported).jobs.build.steps[0].input).toEqual(input);
  let removalMessage = '';
  page.once('dialog', async dialog => { removalMessage = dialog.message(); await dialog.dismiss(); });
  await ui.getByLabel('Protocol', { exact: true }).selectOption('');
  expect(removalMessage).toContain('output-schema');
  expect((await snapshot(page)).workflow.jobs.build.steps[0]).toEqual(helper);
  page.once('dialog', dialog => dialog.accept());
  await ui.getByLabel('Protocol', { exact: true }).selectOption('');
  expect((await snapshot(page)).workflow.jobs.build.steps[0].protocol).toBeUndefined();
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0]).toEqual(helper);
  await job(page);
  await ui.getByRole('button', { name: '2. read', exact: true }).click();
  await expect(ui.locator('.aw-inspector-content > datalist option[value="${{ steps.helper.outputs.head }}"]')).toHaveCount(1);
});

test('wire a helper output into another job through the result picker', async ({ page }) => {
  const source = `schemas: {Output: {type: object, properties: {head: {type: string}}}}
jobs: {build: {container: ubuntu:24.04, steps: [{id: helper, argv: [helper], protocol: action-v1, effect: read_only, input-schema: {type: object}, output-schema: Output}]}, consume: {steps: [{id: read, argv: [echo, '']}]}}`;
  const ui = await open(page, source); await job(page, 'consume');
  await ui.getByRole('button', { name: '1. read', exact: true }).click();
  const before = (await snapshot(page)).source;
  await ui.getByRole('button', { name: 'Use job result in Argument 1', exact: true }).click();
  const picker = ui.getByRole('dialog', { name: 'Use job result', exact: true });
  await picker.getByLabel('Result source', { exact: true }).selectOption('result');
  await expect(picker.locator('datalist option').filter({ hasText: /^outputs\.head$/ })).toHaveCount(1);
  await picker.getByLabel('Result field', { exact: true }).fill('outputs.head');
  await picker.getByRole('button', { name: 'Preview result wiring', exact: true }).click();
  await expect(picker.getByLabel('Result wiring preview', { exact: true })).toContainText('${{ needs.build.steps.helper.outputs.head }}');
  expect((await snapshot(page)).source).toBe(before);
  await picker.getByRole('button', { name: 'Apply job result', exact: true }).click();
  const consumer = (await snapshot(page)).workflow.jobs.consume;
  expect(consumer.needs).toEqual(['build']);
  expect(consumer.steps[0].argv[1]).toBe('${{ needs.build.steps.helper.outputs.head }}');
  expect(parse(await downloadText(page)).jobs.consume.steps[0].argv[1]).toBe('${{ needs.build.steps.helper.outputs.head }}');
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(before);
});

for (const handler of ['onError', 'onFailure', 'finally']) {
  test(`reject a structured helper inside ordinary ${handler} handlers`, async ({ page }) => {
    const source = `jobs: {build: {container: ubuntu:24.04, steps: [{argv: [echo, ready], ${handler}: [{argv: [helper], protocol: action-v1, effect: external_mutation, input-schema: {type: object}, output-schema: {type: object}}]}]}}`;
    const ui = await open(page, source);
    await expect(ui.locator('.aw-diagnostics')).toContainText('Structured helpers cannot own or appear inside ordinary lifecycle handlers');
  });
}

test('repair missing schema names and reject interpolation in structured commands', async ({ page }) => {
  const source = `jobs: {build: {container: ubuntu:24.04, steps: [{argv: [helper, '\${{ inputs.business }}'], protocol: action-v1, effect: read_only, input-schema: Missing, output-schema: {type: object}}]}}`;
  const ui = await open(page, source);
  await expect(ui.locator('.aw-diagnostics')).toContainText('Structured executable commands and settings must be static');
  await expect(ui.locator('.aw-diagnostics')).toContainText('known workflow-local name');
  await ui.getByRole('button', { name: 'YAML', exact: true }).click();
  await ui.getByRole('textbox', { name: 'Workflow YAML' }).fill(source.replace(", '${{ inputs.business }}'", '').replace('input-schema: Missing', 'input-schema: {type: object}'));
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
});
