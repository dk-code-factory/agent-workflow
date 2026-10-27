import { expect, test } from '@playwright/test';
import { open, snapshot, field } from './harness';

test('define callable inputs secrets and outputs', async ({ page }) => {
  const ui = await open(page);
  await ui.getByRole('checkbox', { name: 'workflow_call', exact: true }).check();
  await ui.getByText('Callable inputs', { exact: true }).click();
  await ui.getByRole('button', { name: 'Add input', exact: true }).click();
  await field(page, 'Identifier: input', 'task');
  await ui.getByLabel('Input type: task', { exact: true }).selectOption('boolean');
  await ui.getByLabel('Default: task', { exact: true }).selectOption('true');
  await ui.getByRole('checkbox', { name: 'Required: task', exact: true }).check();
  await ui.getByText('Callable secrets', { exact: true }).click();
  await ui.getByRole('button', { name: 'Add secret', exact: true }).click();
  await field(page, 'Identifier: secret', 'token');
  await ui.getByText('Callable outputs', { exact: true }).click();
  await ui.getByRole('button', { name: 'Add output', exact: true }).click();
  await field(page, 'Output value: output', '${{ jobs.build.outputs.report }}');
  expect((await snapshot(page)).workflow.on.workflow_call).toEqual({ inputs: { task: { type: 'boolean', required: true, default: true } }, secrets: { token: { required: true } }, outputs: { output: { value: '${{ jobs.build.outputs.report }}' } } });
});

test('typed defaults retain literal values and offer field YAML for complex values', async ({ page }) => {
  const ui = await open(page, 'on: {workflow_call: {inputs: {count: {type: number, required: false}, title: {type: string}, config: {type: object}}}}\njobs: {build: {steps: [{argv: [echo]}]}}');
  await ui.getByText('Callable inputs', { exact: true }).click();
  await field(page, 'Default: count', '2.5');
  await field(page, 'Default: title', 'false');
  await field(page, 'Default: config (YAML)', 'items: [one, two]');
  const inputs = (await snapshot(page)).workflow.on.workflow_call.inputs;
  expect(inputs.count).toEqual({ type: 'number', required: false, default: 2.5 });
  expect(inputs.title.default).toBe('false');
  expect(inputs.config.default).toEqual({ items: ['one', 'two'] });
  await expect(ui.getByRole('button', { name: 'Use job result in Default: title', exact: true })).toHaveCount(0);
});
