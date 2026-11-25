import { expect, test } from '@playwright/test';
import { open, snapshot, field, downloadText } from './harness';
import { parseWorkflow } from '../../../src/validation';

const source = `step-definitions:
  greet:
    parameters: {message: {type: string, required: true}}
    step: {argv: [echo, '\${{ parameters.message }}']}
jobs:
  build:
    steps:
      - {id: first, use-definition: greet, with: {message: hello}}
      - {id: second, use-definition: greet, with: {message: world}}
`;

test('reuse declarations and instance bindings survive inspection, export and history', async ({ page }) => {
  const ui = await open(page, source);
  await ui.getByRole('button', { name: 'first definition: greet', exact: true }).click();
  await expect(ui.getByLabel('Step type', { exact: true })).toHaveValue('definition');
  await expect(ui.getByLabel('Step definition', { exact: true })).toHaveValue('greet');
  await field(page, 'Name', 'First invocation');
  const edited = await snapshot(page);
  expect(edited.workflow['step-definitions'].greet.step.argv).toEqual(['echo', '${{ parameters.message }}']);
  expect(edited.workflow.jobs.build.steps[0].with.message).toBe('hello');
  expect(edited.diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  const exported = await downloadText(page);
  expect(exported).toBe(edited.source);
  expect(parseWorkflow(exported).workflow?.['step-definitions']).toEqual(edited.workflow['step-definitions']);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(source);
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(edited.source);
  // Definition reference edits diagnose and recover without replacing the body.
  await field(page, 'Step definition', 'absent');
  expect((await snapshot(page)).diagnostics.some((d: any) => d.code === 'step-definition' && d.severity === 'error')).toBe(true);
  await field(page, 'Step definition', 'greet');
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  const beforeConversion = (await snapshot(page)).source;
  page.once('dialog', dialog => dialog.accept());
  await ui.getByLabel('Step type', { exact: true }).selectOption('argv');
  expect((await snapshot(page)).workflow.jobs.build.steps[0]['use-definition']).toBeUndefined();
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(beforeConversion);
});
