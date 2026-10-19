import { expect, test } from '@playwright/test';
import { open, snapshot, field } from './harness';

test('edit name triggers and advanced interface mapping', async ({ page }) => {
  const ui = await open(page);
  await ui.getByRole('button', { name: 'Workflow properties', exact: true }).click();
  await field(page, 'Name', 'My reusable workflow');
  await ui.getByRole('checkbox', { name: 'workflow_call', exact: true }).check();
  await ui.getByRole('checkbox', { name: 'manual', exact: true }).uncheck();
  expect((await snapshot(page)).workflow.on).toEqual({ workflow_call: {} });
  await expect(ui.getByLabel('Name', { exact: true })).toHaveValue('My reusable workflow');
  await ui.getByText('Advanced trigger and interface YAML', { exact: true }).click();
  await field(page, 'Triggers and callable interface (YAML)', 'manual: {}');
  expect((await snapshot(page)).workflow.on).toEqual({ manual: {} });
});
