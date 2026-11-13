import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '@playwright/test';

test('expression insertion replaces selection and keeps typing at the insertion point', async ({ page }) => {
  await page.goto('/test/browser.html'); const editor = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: `on: { workflow_call: { inputs: { task: { type: string } } } }
jobs: { build: { steps: [ { id: greet, uses: agent/run, with: { prompt: "Before replace after", agent: claude.toml } } ] } }
` }));
  await editor.getByRole('button', { name: 'greet', exact: false }).first().click();
  const prompt = editor.getByLabel('Prompt', { exact: true });
  await prompt.focus(); await prompt.press('Home');
  for (let index = 0; index < 7; index++) await prompt.press('ArrowRight');
  for (let index = 0; index < 7; index++) await prompt.press('Shift+ArrowRight');
  await page.clock.install();
  await page.clock.pauseAt(new Date(Date.now() + 1000));
  await editor.getByLabel('Insert expression into Prompt', { exact: true }).selectOption('inputs.task');
  await expect(prompt).toBeFocused();
  const insertionEnd = 'Before ${{ inputs.task }}'.length;
  expect(await prompt.evaluate((element: HTMLTextAreaElement) => [element.selectionStart, element.selectionEnd])).toEqual([insertionEnd, insertionEnd]);
  for (let index = 0; index < '${{ inputs.task }}'.length; index++) await prompt.press('Shift+ArrowLeft');
  await editor.getByLabel('Insert expression into Prompt', { exact: true }).selectOption('inputs.task');
  await expect(prompt).toBeFocused();
  expect(await prompt.evaluate((element: HTMLTextAreaElement) => [element.selectionStart, element.selectionEnd])).toEqual([insertionEnd, insertionEnd]);
  await prompt.press('!'); await prompt.press('Tab');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.steps[0].with.prompt)).toBe('Before ${{ inputs.task }}! after');
});

test('YAML expression completion follows the cursor and only offers preceding steps', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await first.getByRole('button', { name: 'YAML', exact: true }).click();
  const yaml = first.getByRole('textbox', { name: 'Workflow YAML' });
  await yaml.fill(`jobs:
  build:
    steps:
      - id: earlier
        argv: [ echo ]
      - id: current
        uses: agent/run
        with:
          agent: claude.toml
          prompt: |
            \${{ steps.`);
  await yaml.press('Control+Space');
  await expect(first.getByRole('option', { name: 'steps.earlier.outputs.stdout', exact: true })).toBeVisible();
  await expect(first.getByRole('option', { name: 'steps.earlier.text', exact: true })).toHaveCount(0);
  await expect(first.getByRole('option', { name: 'steps.current.outputs.stdout', exact: true })).toHaveCount(0);
  await first.getByRole('option', { name: 'steps.earlier.outputs.stdout', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().source))
    .toContain('${{ steps.earlier.outputs.stdout');
});

test('property expression hints are limited to fields the runner evaluates', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: (window as any).first.getSnapshot().source.replace('on: { manual: {} }', 'on: {manual: {}, workflow_call: {inputs: {task: {type: string}}}}') }));
  await expect(first.getByLabel('Name', { exact: true })).not.toHaveAttribute('list');
  await first.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await expect(first.getByLabel('Runner', { exact: true })).not.toHaveAttribute('list');
  await expect(first.getByLabel('Condition', { exact: true })).toHaveAttribute('list', /suggestions/);
  await first.getByRole('button', { name: 'greet', exact: false }).first().click();
  await first.getByLabel('Insert expression into Prompt', { exact: true }).selectOption('inputs.task');
  await expect(first.getByLabel('Prompt', { exact: true })).toContainText('${{ inputs.task }}');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.steps[0].with.prompt)).toContain('${{ inputs.task }}');
  expect((await new AxeBuilder({ page }).include('#a').withTags(['wcag2a', 'wcag2aa', 'wcag21aa']).analyze()).violations).toEqual([]);
  await expect(first.getByLabel('Action target', { exact: true })).not.toHaveAttribute('list');
});

test('failure classifications do not offer a conflicting business-field alias', async ({ page }) => {
  await page.goto('/test/browser.html'); const editor = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: JSON.stringify({ schemas: {
    Review: { type: 'object', properties: { approved: { type: 'boolean' }, failure_class: { type: 'string' }, unsafe_failure_class: { type: 'string' } } },
  }, jobs: { review: { steps: [
    { id: 'analyze', uses: 'agent/run', with: { agent: 'claude.toml', prompt: 'Review', output: { format: 'json', schema: 'Review' } } },
    { id: 'finish', uses: 'agent/run', with: { agent: 'claude.toml', prompt: 'Next' } },
  ] } } }) }));
  await editor.getByRole('button', { name: 'Inspect job review', exact: true }).click();
  await editor.getByRole('button', { name: '2. finish', exact: true }).click();
  const options = editor.getByLabel('Insert expression into Prompt', { exact: true });
  await expect(options.locator('option[value="steps.analyze.approved"]')).toHaveCount(1);
  await expect(options.locator('option[value="steps.analyze.failure_class"]')).toHaveCount(0);
  await expect(options.locator('option[value="steps.analyze.unsafe_failure_class"]')).toHaveCount(0);
  const prompt = editor.getByLabel('Prompt', { exact: true });
  await prompt.fill('${{ steps.analyze.failure_class }} / ${{ steps.analyze.outputs.structured.failure_class }}');
  await prompt.press('Tab');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.review.steps[1].with.prompt))
    .toBe('${{ steps.analyze.failure_class }} / ${{ steps.analyze.outputs.structured.failure_class }}');
});

test('concise result suggestions and operand diagnostics follow runner scopes', async ({ page }) => {
  await page.goto('/test/browser.html'); const editor = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: JSON.stringify({ schemas: { Review: { type: 'object', properties: { approved: { type: 'boolean' } } } }, jobs: {
    review: { steps: [{id: 'analyze', uses: 'agent/run', with: {agent: "claude.toml", output: { format: 'json', schema: 'Review' }, prompt: 'Review'}}, {id: 'finish', uses: 'agent/run', with: {agent: "claude.toml", prompt: 'Next'}}] },
    summarize: { needs: ['review'], steps: [{id: 'summary', uses: 'agent/run', with: {agent: "claude.toml", prompt: 'Read'}}] },
  } }) }));
  await editor.getByRole('button', { name: 'Inspect job review', exact: true }).click();
  await editor.getByRole('button', { name: '2. finish', exact: true }).click();
  const options = editor.getByLabel('Insert expression into Prompt', { exact: true });
  await expect(options.locator('option[value="steps.analyze.approved"]')).toHaveCount(1);
  await expect(options.locator('option[value="steps.finish.text"]')).toHaveCount(0);
  await options.selectOption('steps.analyze.approved');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.review.steps[1].with.prompt)).toContain('${{ steps.analyze.approved }}');
  await editor.getByRole('button', { name: 'Inspect job summarize', exact: true }).click();
  await editor.getByRole('button', { name: '1. summary', exact: true }).click();
  await editor.getByLabel('Insert expression into Prompt', { exact: true }).selectOption('needs.review.steps.analyze.approved');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.summarize.steps[0].with.prompt)).toContain('${{ needs.review.steps.analyze.approved }}');
  await editor.getByRole('button', { name: 'Inspect job review', exact: true }).click();
  await editor.getByRole('button', { name: '1. analyze', exact: true }).click();
  const prompt = editor.getByLabel('Prompt', { exact: true });
  await prompt.fill('${{ steps.finish.$value }}'); await prompt.press('Tab');
  await expect(editor.locator('.aw-diagnostics').getByRole('button', { name: /Step result "finish" is unavailable here/ })).toBeVisible();
  await prompt.fill('${{ "steps.finish.$value" }}'); await prompt.press('Tab');
  await expect(editor.locator('.aw-diagnostics').getByRole('button', { name: /Step result "finish" is unavailable here/ })).toHaveCount(0);
  await prompt.fill(String.raw`${'${{'} '\' == steps.finish.text' }}`); await prompt.press('Tab');
  await expect(editor.locator('.aw-diagnostics').getByRole('button', { name: /Step result "finish" is unavailable here/ })).toBeVisible();
  await prompt.fill('Review'); await prompt.press('Tab');
  const condition = editor.getByLabel('Condition', { exact: true });
  await condition.fill('${{ "}}" == "}}" and steps.finish.text }}'); await condition.press('Tab');
  await expect(editor.locator('.aw-diagnostics').getByRole('button', { name: /Step result "finish" is unavailable here/ })).toBeVisible();
  await condition.fill('${{ "}}" == "}}" }}'); await condition.press('Tab');
  await expect(editor.locator('.aw-diagnostics').getByRole('button', { name: /Step result "finish" is unavailable here/ })).toHaveCount(0);
});

test('shared-schema suggestions stay bounded and YAML prefixes reach later dependency fields', async ({ page }) => {
  await page.goto('/test/browser.html'); const editor = page.locator('#a');
  const source = await page.evaluate(() => {
    const schemas = { Shared: { type: 'object', properties: Object.fromEntries(Array.from({ length: 50 }, (_, index) => [`field${index}`, { type: 'string' }])) } };
    const jobs = Object.fromEntries(Array.from({ length: 25 }, (_, index) => [`producer${index}`, { steps: [{id: 'analyze', uses: 'agent/run', with: {agent: "claude.toml", output: { schema: 'Shared' }, prompt: 'Review'}}] }]));
    const needs = Object.keys(jobs);
    (window as any).first.update({ source: JSON.stringify({ schemas, jobs: { ...jobs, consumer: { needs, steps: [{id: 'read', uses: 'agent/run', with: {agent: "claude.toml", prompt: 'Read'}}] } } }) });
    return `schemas: ${JSON.stringify(schemas)}\njobs:\n${Object.entries(jobs).map(([id, job]) => `  ${id}: ${JSON.stringify(job)}`).join('\n')}\n  consumer:\n    needs: ${JSON.stringify(needs)}\n    steps:\n      - id: read\n        agent: claude\n        prompt: |\n          ${'${{'} needs.producer24.steps.analyze.field49`;
  });
  await expect(editor.locator('.aw-inspector-content > datalist option')).toHaveCount(200);
  await editor.getByLabel('Search workflow', { exact: true }).fill('read');
  await editor.getByRole('list', { name: 'Workflow search results' }).getByRole('button', { name: 'read Step read · Job consumer', exact: true }).click();
  await expect(editor.getByLabel('Prompt', { exact: true })).toHaveValue('Read');
  await expect(editor.locator('.aw-inspector-content > datalist option')).toHaveCount(200);
  await expect(editor.getByLabel('Insert expression into Prompt', { exact: true }).locator('option')).toHaveCount(201);
  await editor.getByRole('button', { name: 'YAML', exact: true }).click();
  const yaml = editor.getByRole('textbox', { name: 'Workflow YAML' });
  await yaml.fill(source); await yaml.press('Control+Space');
  await expect(editor.getByRole('option', { name: 'needs.producer24.steps.analyze.field49', exact: true })).toBeVisible();
});
