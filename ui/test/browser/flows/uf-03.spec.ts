import { expect, test, type Locator } from '@playwright/test';
import { open, snapshot } from './harness';

const source = `name: Search fixture
jobs:
  build:
    name: Build service
    steps:
      - id: greeting
        name: Shared step
        uses: agent/claude
        with: {prompt: Needle prompt from build}
      - argv: [echo, Needle direct command]
      - id: deploy
        name: Deploy shell
        shell: sh
        run: printf 'Needle shell command'
  test:
    name: Test service
    needs: build
    steps:
      - id: greeting
        name: Shared step
        uses: agent/claude
        with: {prompt: Needle prompt from test}
  docs:
    needs: test
    steps:
      - argv: [echo, documentation]
`;
const results = (ui: Locator) => ui.getByRole('list', { name: 'Workflow search results' });
const search = (ui: Locator) => ui.getByLabel('Search workflow', { exact: true });
const history = (value: any) => ({ source: value.source, revision: value.revision, canUndo: value.canUndo, canRedo: value.canRedo });
async function expectStepInCanvas(ui: Locator, job: string, index: number) {
  const target = ui.locator(`.react-flow__node[data-id="${job}"] .aw-steps > li`).nth(index).locator('button');
  await expect(target).toHaveClass(/aw-active-step/);
  // Page visibility alone misses clipping by the graph's own viewport.
  await expect.poll(async () => {
    const step = await target.boundingBox(); const canvas = await ui.locator('.aw-flow').boundingBox();
    return !!step && !!canvas && step.x >= canvas.x && step.y >= canvas.y && step.x + step.width <= canvas.x + canvas.width && step.y + step.height <= canvas.y + canvas.height;
  }).toBe(true);
}

test('find job metadata and step names, prompts, commands and effective IDs in document order', async ({ page }) => {
  const ui = await open(page, source); const before = history(await snapshot(page));
  await search(ui).fill('  nEeDlE  ');
  await expect(ui.getByRole('status')).toHaveText('4 results');
  await expect(results(ui).getByRole('button')).toHaveText([
    'Shared stepStep greeting · Job Build service (build)',
    'step_2Step step_2 · Job Build service (build)',
    'Deploy shellStep deploy · Job Build service (build)',
    'Shared stepStep greeting · Job Test service (test)',
  ]);
  await expect(ui.locator('.aw-dimmed')).toHaveCount(1);
  await expect(ui.locator('.react-flow__edge')).toHaveCount(2);
  for (const [query, count] of [['service', 2], ['greeting', 2], ['shared step', 2], ['prompt from test', 1], ['direct command', 1], ['shell command', 1], ['step_2', 1]] as const) {
    await search(ui).fill(query);
    await expect(results(ui).getByRole('button')).toHaveCount(count);
    await expect(ui.getByRole('status')).toHaveText(`${count} ${count === 1 ? 'result' : 'results'}`);
  }
  await results(ui).getByRole('button').click();
  await expect(ui.getByLabel('Step ID', { exact: true })).toHaveValue('step_2');
  await expectStepInCanvas(ui, 'build', 1);
  await search(ui).fill('Test service');
  await results(ui).getByRole('button').click();
  await expect(ui.getByLabel('Job ID', { exact: true })).toHaveValue('test');
  await search(ui).fill('not-in-this-workflow');
  await expect(ui.getByRole('status')).toHaveText('No results');
  await expect(results(ui).getByRole('button')).toHaveCount(0);
  await search(ui).press('Escape');
  await expect(search(ui)).toBeFocused();
  await expect(search(ui)).toHaveValue('');
  await expect(results(ui)).toHaveCount(0);
  await expect(ui.locator('.aw-dimmed')).toHaveCount(0);
  await search(ui).fill('   ');
  await expect(results(ui)).toHaveCount(0);
  await expect(ui.locator('.aw-search-count')).toBeEmpty();
  await expect(ui.locator('.aw-dimmed')).toHaveCount(0);
  await ui.getByLabel('Search palette', { exact: true }).fill('Build service');
  await expect(ui.locator('.aw-palette').getByRole('button', { name: 'Test service', exact: true })).toHaveCount(0);
  await expect(ui.locator('.aw-palette').getByRole('button', { name: 'Build service', exact: true })).toBeVisible();
  expect(history(await snapshot(page))).toEqual(before);
});

test('activate duplicate names with Tab, arrows, Enter and Space and reveal YAML without changing history', async ({ page }) => {
  const ui = await open(page, source);
  await ui.getByRole('button', { name: 'Expand test', exact: true }).click();
  await ui.getByRole('button', { name: 'Workflow properties', exact: true }).click();
  await ui.getByLabel('Name', { exact: true }).fill('Temporary workflow title');
  await ui.getByLabel('Name', { exact: true }).press('Tab');
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  const before = history(await snapshot(page)); expect(before.canRedo).toBe(true);
  await search(ui).fill('Shared step');
  const buttons = results(ui).getByRole('button');
  await search(ui).press('Tab'); // Fit view
  await page.keyboard.press('Tab'); // Auto layout
  await page.keyboard.press('Tab');
  await expect(buttons.nth(0)).toBeFocused();
  await page.keyboard.press('ArrowDown'); await expect(buttons.nth(1)).toBeFocused();
  await page.keyboard.press('ArrowDown'); await expect(buttons.nth(0)).toBeFocused();
  await page.keyboard.press('ArrowUp'); await expect(buttons.nth(1)).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(ui.getByLabel('Prompt', { exact: true })).toHaveValue('Needle prompt from test');
  await expect(ui.getByRole('button', { name: 'Expand test', exact: true })).toHaveAttribute('aria-expanded', 'true');
  await expect(buttons.nth(1)).toBeFocused();
  await expectStepInCanvas(ui, 'test', 0);
  await expect(ui.locator('.cm-line').filter({ hasText: 'with: {prompt: Needle prompt from test}' })).toBeInViewport();
  await page.keyboard.press('ArrowUp'); await page.keyboard.press('Space');
  await expect(ui.getByLabel('Prompt', { exact: true })).toHaveValue('Needle prompt from build');
  await expectStepInCanvas(ui, 'build', 0);
  await page.keyboard.press('Escape');
  await expect(search(ui)).toBeFocused(); await expect(search(ui)).toHaveValue('');
  await search(ui).fill('Shared step'); await search(ui).press('ArrowUp');
  await expect(buttons.nth(1)).toBeFocused();
  await search(ui).focus(); await search(ui).press('ArrowDown');
  await expect(buttons.nth(0)).toBeFocused();
  expect(history(await snapshot(page))).toEqual(before);
  await expect(ui.locator('.react-flow__edge')).toHaveCount(2);
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect((await snapshot(page)).workflow.name).toBe('Temporary workflow title');
});

test('expand and center an offscreen step in a long card using updated geometry', async ({ page }) => {
  const longSource = `jobs:\n  long:\n    steps:\n${Array.from({ length: 35 }, (_, i) => `      - id: item_${i}\n        argv: [echo, command ${i}]\n`).join('')}  next:\n    needs: long\n    steps: [{argv: [echo]}]\n`;
  const ui = await open(page, longSource);
  await page.evaluate(() => (window as any).first.update({ layout: { positions: { long: { x: 2200, y: 1800 }, next: { x: 2600, y: 4500 } }, collapsed: { long: true }, viewport: { x: 0, y: 0, zoom: 1 } } }));
  const before = history(await snapshot(page));
  await search(ui).fill('command 34');
  await results(ui).getByRole('button').click();
  await expect(ui.getByLabel('Step ID', { exact: true })).toHaveValue('item_34');
  await expectStepInCanvas(ui, 'long', 34);
  await expect(ui.getByRole('button', { name: 'Expand long', exact: true })).toHaveAttribute('aria-expanded', 'true');
  await expect(ui.locator('.react-flow__edge')).toHaveCount(1);
  expect(history(await snapshot(page))).toEqual(before);
  // Re-selection must reveal the result again after moving the viewport away.
  await ui.getByRole('button', { name: 'Fit view', exact: true }).click();
  await results(ui).getByRole('button').click();
  await expectStepInCanvas(ui, 'long', 34);
});

test('refresh results after form edits, undo and host document replacement', async ({ page }) => {
  const ui = await open(page, source);
  await search(ui).fill('prompt from test');
  await results(ui).getByRole('button').click();
  await ui.getByLabel('Prompt', { exact: true }).fill('Replacement prompt');
  await ui.getByLabel('Prompt', { exact: true }).press('Tab');
  await expect(ui.getByRole('status')).toHaveText('No results');
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  await expect(ui.getByRole('status')).toHaveText('1 result');
  await page.evaluate(() => (window as any).first.update({ source: 'jobs: {replacement: {steps: [{argv: [echo, prompt from test]}]}}' }));
  await expect(results(ui).getByRole('button')).toHaveText(['step_1Step step_1 · Job replacement']);
  await results(ui).getByRole('button').click();
  await expect(ui.getByLabel('Step ID', { exact: true })).toHaveValue('step_1');
  await expectStepInCanvas(ui, 'replacement', 0);
});

test('viewer search expands and inspects steps while preserving the document', async ({ page }) => {
  await page.goto('/test/browser.html?viewer'); const ui = page.locator('#a');
  await page.evaluate(source => (window as any).first.update({ source, layout: { positions: {}, collapsed: { test: true } } }), source);
  const before = history(await snapshot(page));
  await search(ui).fill('prompt from test');
  await search(ui).press('ArrowDown'); await page.keyboard.press('Enter');
  await expect(ui.getByLabel('Prompt', { exact: true })).toHaveValue('Needle prompt from test');
  await expect(ui.getByLabel('Prompt', { exact: true })).toHaveAttribute('readonly', '');
  await expectStepInCanvas(ui, 'test', 0);
  await expect(ui.getByRole('button', { name: 'Expand test', exact: true })).toHaveAttribute('aria-expanded', 'true');
  expect(history(await snapshot(page))).toEqual(before);
});

test('narrow containers scroll results and isolate search, selection and layout per instance', async ({ page }) => {
  const ui = await open(page, source); const second = page.locator('#b');
  await page.evaluate(() => { document.querySelector<HTMLElement>('#a')!.style.width = '500px'; });
  await ui.getByRole('button', { name: 'Visual', exact: true }).click();
  const before = history(await snapshot(page));
  const secondBefore = await page.evaluate(() => (window as any).second.getSnapshot());
  const secondViewport = await second.locator('.react-flow__viewport').getAttribute('style');
  await search(ui).fill('needle');
  await expect(results(ui).getByRole('button')).toHaveCount(4);
  expect(await results(ui).evaluate(element => element.scrollHeight > element.clientHeight)).toBe(true);
  await results(ui).getByRole('button').last().click();
  await expect(ui.getByLabel('Prompt', { exact: true })).toHaveValue('Needle prompt from test');
  await expect(ui.getByRole('heading', { name: 'Step', exact: true })).toBeFocused();
  await ui.getByRole('button', { name: 'canvas', exact: true }).click();
  await expectStepInCanvas(ui, 'test', 0);
  await expect(second.locator('.aw-selected')).toHaveCount(0);
  await expect(search(second)).toHaveValue(''); await expect(results(second)).toHaveCount(0);
  expect(await second.locator('.react-flow__viewport').getAttribute('style')).toBe(secondViewport);
  expect(await page.evaluate(() => (window as any).second.getSnapshot())).toEqual(secondBefore);
  await expect(ui.locator('.aw-editor')).toHaveJSProperty('scrollWidth', await ui.locator('.aw-editor').evaluate(element => element.clientWidth));
  expect(history(await snapshot(page))).toEqual(before);
  await page.evaluate(() => { const element = document.querySelector<HTMLElement>('#a')!; element.style.width = '320px'; element.style.height = '480px'; });
  await ui.getByRole('button', { name: 'Split', exact: true }).click();
  await expect.poll(async () => (await ui.locator('.aw-flow').boundingBox())?.height ?? 0).toBeGreaterThan(0);
  await expect(results(ui).getByRole('button').last()).toBeAttached();
});
