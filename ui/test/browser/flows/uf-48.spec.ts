import { expect, test } from '@playwright/test';
import { open, step, field, snapshot, downloadText } from './harness';

test('edit native definitions and typed input while retaining six visible hook lists', async ({ page }) => {
  const source=`jobs:
  review:
    steps:
      - null
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
  await expect(ui.getByLabel('Task input (YAML)',{exact:true})).toHaveValue(/count: 7/);
  await field(page,'Agent definition or reference','../.codex/agents/reviewer.toml');
  await field(page,'Agent turn allowance','0');
  expect((await snapshot(page)).diagnostics.some((d:any)=>d.path.at(-1)==='turns' && d.severity==='error')).toBe(true);
  await field(page,'Agent turn allowance','5');
  await field(page,'Task input (YAML)','{count: 9, enabled: false, exact: 9007199254740993}');
  const edited=await snapshot(page);const task=edited.workflow.jobs.review.steps[0];
  expect(task.with.turns).toBe(5);
  expect(task.with.input.count).toBe(9);expect(task.with.input.enabled).toBe(false);
  expect(edited.source).toContain('9007199254740993');
  await expect(ui.getByLabel('Task input (YAML)',{exact:true})).toHaveValue(/9007199254740993/);
  for(const key of ['onResult','onComplete','onIncomplete','onError','onFailure','finally'])expect(task[key]).toHaveLength(1);
  await ui.getByText('Failure handlers and finalization',{exact:true}).click();
  for (const [label, command] of [
    ['Result verification steps (YAML)','verify.py'], ['Completion steps (YAML)','accepted'],
    ['Incomplete steps (YAML)','feedback'], ['Fallback steps (YAML)','recover'],
    ['Failure reporting steps (YAML)','report'], ['Finalization steps (YAML)','finalize'],
  ]) {
    await expect(ui.getByLabel(label,{exact:true})).toBeVisible();
    await expect(ui.getByLabel(label,{exact:true})).toHaveValue(new RegExp(command));
  }

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

test('hook YAML edits preserve exact nested numeric input through prompt changes and export', async ({ page }) => {
  const hooks = ['onResult', 'onComplete', 'onIncomplete', 'onError', 'onFailure', 'finally'];
  const labels = ['Result verification steps (YAML)', 'Completion steps (YAML)', 'Incomplete steps (YAML)', 'Fallback steps (YAML)', 'Failure reporting steps (YAML)', 'Finalization steps (YAML)'];
  const source = `jobs:
  review:
    steps:
      - id: review
        uses: agent/run
        with:
          agent: reviewer.toml
          prompt: Review
          input: {fraction: 0.10000000000000001, exponent: 1.234567890123456789e-20}
          output: {format: json, schema: {type: object, required: [complete], properties: {complete: {type: boolean}}}}
${hooks.map(hook => `        ${hook}:
          - uses: agent/run
            with: {agent: reviewer.toml, prompt: Verify, input: {exact: 9007199254740993, fraction: 1.0000000000000001}}
`).join('')}`;
  const ui = await open(page, source); await step(page, 'review agent/run');
  await expect(ui.getByLabel('Task input (YAML)', {exact:true})).toHaveValue(/0\.10000000000000001/);
  await ui.getByText('Failure handlers and finalization', {exact:true}).click();
  for (const label of labels) {
    const control = ui.getByLabel(label, {exact:true});
    const before = await control.inputValue();
    expect(before).toContain('9007199254740993');
    expect(before).toContain('1.0000000000000001');
    await field(page, label, before.replace('prompt: Verify', 'prompt: Revised'));
    await expect(control).toHaveValue(/prompt: Revised/);
    await expect(control).toHaveValue(/9007199254740993/);
    await expect(control).toHaveValue(/1\.0000000000000001/);
  }
  await field(page, 'Prompt', 'Review carefully');
  const edited = await snapshot(page);
  for (const token of ['9007199254740993', '0.10000000000000001', '1.0000000000000001', '1.234567890123456789e-20']) expect(edited.source).toContain(token);
  for (const hook of hooks) expect(edited.workflow.jobs.review.steps[0][hook][0].with.prompt).toBe('Revised');
  expect(edited.diagnostics.filter((d:any) => d.severity === 'error')).toEqual([]);
  expect(await downloadText(page)).toBe(edited.source);
  await ui.getByRole('button', {name:'Undo', exact:true}).click();
  expect((await snapshot(page)).workflow.jobs.review.steps[0].with.prompt).toBe('Review');
  await ui.getByRole('button', {name:'Redo', exact:true}).click();
  expect((await snapshot(page)).source).toBe(edited.source);
});
