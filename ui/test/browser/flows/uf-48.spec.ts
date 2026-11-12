import { expect, test } from '@playwright/test';
import { open, step, field, snapshot, downloadText } from './harness';

test('edit native definitions and typed input while retaining six visible hook lists', async ({ page }) => {
  const source=`jobs:
  review:
    steps:
      - id: review
        uses: agent/run
        with:
          agent: reviewer.toml
          prompt: Review
          input: {count: 7, enabled: true}
          output: {format: json, schema: {type: object, required: [complete], properties: {complete: {type: boolean}}}}
        onResult: [{argv: [python3, verify.py]}]
        onComplete: [{argv: [echo, accepted]}]
        onIncomplete: [{argv: [echo, feedback]}]
        onError: [{argv: [echo, recover]}]
        onFailure: [{argv: [echo, report]}]
        finally: [{argv: [echo, finalize]}]
`;
  const ui=await open(page,source);await step(page,'review agent/run');
  await field(page,'Agent definition or reference','../.codex/agents/reviewer.toml');
  await field(page,'Task input (YAML)','{count: 9, enabled: false}');
  const edited=await snapshot(page);const task=edited.workflow.jobs.review.steps[0];
  expect(task.with.input).toEqual({count:9,enabled:false});
  for(const key of ['onResult','onComplete','onIncomplete','onError','onFailure','finally'])expect(task[key]).toHaveLength(1);
  await ui.getByRole('button',{name:'Undo',exact:true}).click();
  expect((await snapshot(page)).workflow.jobs.review.steps[0].with.input.count).toBe(7);
  await ui.getByRole('button',{name:'Redo',exact:true}).click();
  expect((await snapshot(page)).source).toBe(edited.source);
  const exported=await downloadText(page);expect(exported).toBe(edited.source);
  page.once('dialog',async dialog=>{expect(dialog.message()).toContain('onResult');await dialog.accept();});
  await ui.getByLabel('Step type',{exact:true}).selectOption('argv');
  expect((await snapshot(page)).workflow.jobs.review.steps[0].onResult).toBeUndefined();
  await ui.getByRole('button',{name:'Undo',exact:true}).click();expect((await snapshot(page)).source).toBe(edited.source);
});
