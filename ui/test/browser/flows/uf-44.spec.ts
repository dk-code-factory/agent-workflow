import { expect, test } from '@playwright/test';
import { open, snapshot, field, job, step } from './harness';

test('restore the native runtime catalog and correct a workflow reference', async ({ page }) => {
  const ui = await open(page, `jobs: { build: { steps: [ { id: greet, uses: agent/run, with: { prompt: Keep, agent: missing.toml, input: { count: 7 }, turns: 2 } } ] }, call: { uses: workflows/missing.yml } }
`);
  await page.evaluate(() => (window as any).first.update({ agents: [{ id: 'claude' }], workflows: [{ id: 'workflows/available.yml' }] }));
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.code === 'agent-catalog')).toHaveLength(1);
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.code === 'workflow-catalog')).toHaveLength(1);
  await step(page, 'greet agent/run'); await field(page, 'Agent definition or reference', 'reviewer.toml');
  const edited = await snapshot(page);
  expect(edited.workflow.jobs.build.steps[0]).toMatchObject({ uses: 'agent/run', with: { agent: 'reviewer.toml', prompt: 'Keep', input: { count: 7 }, turns: 2 } });
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with).toEqual({ agent: 'missing.toml', prompt: 'Keep', input: { count: 7 }, turns: 2 });
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(edited.source);
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.code === 'agent-catalog')).toHaveLength(1);
  await page.evaluate(() => (window as any).first.update({ agents: [{ id: 'claude' }, { id: 'codex' }] }));
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.code === 'agent-catalog')).toEqual([]);
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.code === 'workflow-catalog')).toHaveLength(1);
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.prompt).toBe('Keep');
  await job(page, 'call'); await field(page, 'Reusable workflow', 'workflows/available.yml');
  expect((await snapshot(page)).diagnostics.filter((d: any) => /catalog|unavailable|unknown-agent|unknown-workflow/.test(d.code))).toEqual([]);
  await expect(ui.locator('.aw-diagnostics')).not.toContainText('unavailable');
});
