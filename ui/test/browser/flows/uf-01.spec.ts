import { expect, test } from '@playwright/test';

for (const viewer of [false, true]) test(`${viewer ? 'viewer' : 'editor'} selection reveals YAML without changing source or stealing focus`, async ({ page }) => {
  const errors: string[] = []; page.on('pageerror', error => errors.push(error.message));
  await page.goto(`/test/browser.html${viewer ? '?viewer' : ''}`);
  const editor = page.locator('#a');
  const text = `${'# Header padding\n'.repeat(80)}jobs:\n  target:\n    steps:\n      - id: first\n        shell: sh\n        run: |\n${'          echo padding\n'.repeat(60)}      - id: second\n        argv: [echo, selected]\n`;
  const source = viewer ? text.replaceAll('\n', '\r\n') : text;
  await page.evaluate(source => (window as any).first.update({ source }), source);
  const before = await page.evaluate(() => (window as any).first.getSnapshot());
  const job = editor.getByRole('button', { name: 'Inspect job target', exact: true });
  await job.click();
  await expect(editor.locator('.cm-line').filter({ hasText: /^  target:$/ })).toBeInViewport();
  await expect(job).toBeFocused();
  const step = editor.getByRole('button', { name: 'second direct command', exact: true });
  await step.click();
  const definition = editor.locator('.cm-line').filter({ hasText: /- id: second/ });
  await expect(definition).toBeInViewport();
  await expect(step).toBeFocused();
  await expect(editor.locator('.cm-selectionBackground').first()).toBeVisible();
  // Re-selecting the same element must reveal it again after manual scrolling.
  await editor.locator('.cm-scroller').evaluate(element => { element.scrollTop = 0; });
  await expect(definition).not.toBeInViewport();
  await step.click();
  await expect(definition).toBeInViewport();
  // Selections made while YAML is hidden are revealed when it is opened.
  await editor.getByRole('button', { name: 'Visual', exact: true }).click();
  await editor.locator('.aw-palette').getByRole('button', { name: 'target', exact: true }).click();
  await editor.getByRole('button', { name: '2. second', exact: true }).click();
  await editor.getByRole('button', { name: 'YAML', exact: true }).click();
  await expect(definition).toBeInViewport();
  const after = await page.evaluate(() => (window as any).first.getSnapshot());
  expect(after.source).toBe(before.source);
  expect(after.revision).toBe(before.revision);
  expect(after.canUndo).toBe(before.canUndo);
  expect(errors).toEqual([]);
});
