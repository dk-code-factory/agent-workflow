import { expect, test } from '@playwright/test';

for (const theme of ['light', 'dark'] as const) test(`narrow ${theme} YAML search keeps labels visible and replaces source`, async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(theme => {
    const element = document.querySelector<HTMLElement>('#a')!;
    element.style.width = '320px'; element.style.height = '700px';
    (window as any).first.update({ theme });
  }, theme);
  await first.getByRole('button', { name: 'YAML', exact: true }).click();
  await first.getByRole('textbox', { name: 'Workflow YAML' }).press('ControlOrMeta+f');
  const search = first.locator('.cm-search');
  await search.getByRole('textbox', { name: 'Find', exact: true }).fill('Hello');
  for (const name of ['match case', 'regexp', 'by word']) {
    const checkbox = search.getByRole('checkbox', { name, exact: true });
    await checkbox.check(); await expect(checkbox).toBeChecked();
    const label = await checkbox.locator('..').boundingBox(); const panel = await search.boundingBox();
    expect(label!.x + label!.width).toBeLessThanOrEqual(panel!.x + panel!.width);
  }
  expect(await search.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
  await search.getByRole('textbox', { name: 'Replace', exact: true }).fill('Updated');
  // CodeMirror's default text fields are light even inside a dark host. Check
  // the actual typed controls so an upstream theme cannot obscure their text.
  for (const name of ['Find', 'Replace']) {
    const colors = await search.getByRole('textbox', { name, exact: true }).evaluate(element => {
      const field = getComputedStyle(element);
      const panel = getComputedStyle(element.closest('.cm-panels')!);
      return { text: field.color, background: field.backgroundColor, panelText: panel.color, panelBackground: panel.backgroundColor };
    });
    expect(colors.text).toBe(colors.panelText);
    expect(colors.background).toBe(colors.panelBackground);
  }
  await search.getByRole('button', { name: 'replace all', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.steps[0].with.prompt)).toBe('Updated');
  await search.getByRole('button', { name: 'close', exact: true }).click();
  await expect(first.getByRole('textbox', { name: 'Workflow YAML' })).toBeFocused();
});
