import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, writeFileSync, rmSync, symlinkSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { execFileSync, spawnSync } from 'node:child_process';
import { analyze, revision, baseline, discovery, checkDiscovered, syntax, prose, report, specPath } from './flows-check.mjs';

const moduleDir = import.meta.dirname;
const git = (root, ...args) => execFileSync('git', ['-C', root, ...args], { encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'] }).trim();
const doc = (id = '01', text = 'Inspect a workflow.') => `### UF-${id} — Flow ${id}\n\n**UI tests**: [UI tests](../ui/test/browser/flows/uf-${id}.spec.ts)\n\n${text}\n`;
const spec = "import { test, expect } from '@playwright/test';\ntest('flow', async () => { expect(1).toBe(1); });\n";
function fixture(t) {
  const root = mkdtempSync(path.join(tmpdir(), 'ui-flows-')); t.after(() => rmSync(root, { recursive: true, force: true }));
  function put(file, source) { mkdirSync(path.dirname(path.join(root, file)), { recursive: true }); writeFileSync(path.join(root, file), source); }
  git(root, 'init', '-b', 'main'); git(root, 'config', 'user.name', 'Test'); git(root, 'config', 'user.email', 'test@example.invalid');
  put('.gitignore', 'ui/node_modules\nui/test-results\n');
  put('docs/UI_USER_FLOWS.md', doc() + '\n' + doc('02'));
  put(specPath('UF-01'), spec); put(specPath('UF-02'), spec);
  put('ui/test/browser.html', '<script src="/test/browser.tsx"></script>'); put('ui/test/browser.tsx', 'export const fixture = 1;');
  put('ui/package.json', '{"type":"module"}');
  put('ui/playwright.config.ts', "export default { testDir: './test/browser', projects: [{name: 'chromium'}], forbidOnly: true, reporter: [['list'], [" + JSON.stringify(path.join(moduleDir, 'flow-reporter.mjs')) + "]] };");
  symlinkSync(path.resolve(moduleDir, '../node_modules'), path.join(root, 'ui/node_modules'), 'dir');
  git(root, 'add', '.'); git(root, 'commit', '-qm', 'baseline'); const base = git(root, 'rev-parse', 'HEAD');
  return { root, put, base, check: () => analyze(revision(root), revision(root, baseline(root, { base }))) };
}
const missing = result => result.findings.map(f => `${f.id}: ${f.missing}`).join('\n');

test('valid mapping and unrelated edits select no browsers', t => {
  const f = fixture(t); f.put('notes.txt', 'unrelated'); const result = f.check();
  assert.deepEqual(result.findings, []); assert.deepEqual(result.selected, []);
  assert.deepEqual(checkDiscovered(result.sections, discovery(f.root)), []);
});
test('missing, duplicate, malformed, orphan and broken links are actionable', t => {
  const f = fixture(t); f.put('docs/UI_USER_FLOWS.md', doc() + doc() + '\n### UF-2 — Bad\n');
  rmSync(path.join(f.root, specPath('UF-01')));
  const result = f.check(); assert.match(missing(result), /unique UF ID/); assert.match(missing(result), /valid UF heading/); assert.match(missing(result), /owning Playwright spec/); assert.match(missing(result), /owning UF section/);
  f.put('docs/UI_USER_FLOWS.md', doc().replace('uf-01.spec.ts', 'uf-02.spec.ts'));
  assert.match(missing(f.check()), /correct UI tests link/);
});
for (const edit of ['unstaged', 'staged', 'committed']) test(`${edit} UF-only and spec-only edits fail; paired edits pass`, t => {
  const f = fixture(t); const finalize = () => { if (edit !== 'unstaged') git(f.root, 'add', '.'); if (edit === 'committed') git(f.root, 'commit', '-qm', 'edit'); };
  f.put('docs/UI_USER_FLOWS.md', doc('01', 'Inspect the selected job.') + doc('02')); finalize();
  assert.match(missing(f.check()), /UF-01: substantive change/);
  f.put(specPath('UF-01'), spec.replace('toBe(1)', 'toEqual(1)')); finalize(); assert.deepEqual(f.check().findings, []); assert.deepEqual(f.check().selected, ['UF-01']);
  f.put(specPath('UF-02'), spec.replace('expect(1)', 'expect(2)')); finalize(); assert.match(missing(f.check()), /UF-02: substantive update/);
});
test('changes to another section cannot acknowledge the owning test', t => {
  const f = fixture(t); f.put(specPath('UF-01'), spec.replace('toBe(1)', 'toEqual(1)'));
  f.put('docs/UI_USER_FLOWS.md', doc() + doc('02', 'Coverage: checks restored cursor position.'));
  assert.match(missing(f.check()), /UF-01: substantive update/); assert.match(missing(f.check()), /UF-02: substantive change/);
});
test('formatting, comments, Markdown formatting and link-only edits are not pairing', t => {
  assert.equal(syntax('const n = "a";'), syntax("// note\nconst n='a'"));
  assert.equal(syntax('const x = { a: 1, };'), syntax('const x={a:1}'));
  assert.equal(prose(doc()), prose(doc().replace('Inspect', '**Inspect**').replace('uf-01.spec.ts', 'other.ts')));
  assert.equal(prose(doc()), prose(doc() + '<!-- revised coverage -->'));
  assert.notEqual(prose('workflow_call'), prose('workflowcall'));
  const f = fixture(t); f.put(specPath('UF-01'), spec + '// more explanation\n');
  f.put('docs/UI_USER_FLOWS.md', doc('01', 'New behavior') + doc('02'));
  assert.match(missing(f.check()), /substantive change/);
  f.put(specPath('UF-01'), spec.replace('toBe(1)', 'toEqual(1)')); f.put('docs/UI_USER_FLOWS.md', doc().replace('Inspect', '**Inspect**') + doc('02'));
  assert.match(missing(f.check()), /substantive update/);
});
test('shared imported helpers affect each dependent UF and group repairs', t => {
  const f = fixture(t);
  f.put('ui/test/browser/shared.ts', 'export const value = 1;');
  for (const id of ['UF-01', 'UF-02']) f.put(specPath(id), "import { value } from '../shared';\n" + spec);
  git(f.root, 'add', '.'); git(f.root, 'commit', '-qm', 'helper'); f.base = git(f.root, 'rev-parse', 'HEAD');
  f.put('ui/test/browser/shared.ts', 'export const value = 2;');
  const result = analyze(revision(f.root), revision(f.root, f.base));
  assert.deepEqual(result.selected, ['UF-01', 'UF-02']); assert.equal(result.findings.length, 2); assert.match(result.findings[0].group, /UF-01, UF-02/);
  f.put('docs/UI_USER_FLOWS.md', doc('01', 'Coverage: checks updated helper input.') + doc('02', 'Coverage: checks updated helper input.'));
  assert.deepEqual(analyze(revision(f.root), revision(f.root, f.base)).findings, []);
});
test('removed imports and deleted dependencies are inspected in both revisions', t => {
  const f = fixture(t); f.put('ui/test/browser/shared.ts', 'export const value = 1;'); f.put(specPath('UF-01'), "import { value } from '../shared';\n" + spec);
  git(f.root, 'add', '.'); git(f.root, 'commit', '-qm', 'helper'); const base = git(f.root, 'rev-parse', 'HEAD');
  rmSync(path.join(f.root, 'ui/test/browser/shared.ts')); f.put(specPath('UF-01'), spec);
  const result = analyze(revision(f.root), revision(f.root, base)); assert.match(missing(result), /UF-01: substantive update/); assert.match(result.findings[0].file, /shared.ts/);
});
test('shared HTML and TSX fixtures affect every harness flow', t => {
  const f = fixture(t); f.put('ui/test/browser.html', '<main>Changed fixture</main>');
  assert.deepEqual(f.check().selected, ['UF-01', 'UF-02']); assert.equal(f.check().findings.length, 2);
});
test('paired untracked additions and paired removals pass', t => {
  const f = fixture(t); f.put('docs/UI_USER_FLOWS.md', doc() + doc('02') + doc('03')); f.put(specPath('UF-03'), spec);
  assert.deepEqual(f.check().findings, []); assert.deepEqual(f.check().selected, ['UF-03']);
  f.put('docs/UI_USER_FLOWS.md', doc('02') + doc('03')); rmSync(path.join(f.root, specPath('UF-01')));
  assert.deepEqual(f.check().findings, []); assert.deepEqual(f.check().selected, ['UF-03']);
});
test('one-sided removals fail', t => {
  const f = fixture(t); f.put('docs/UI_USER_FLOWS.md', doc('02'));
  assert.match(missing(f.check()), /removal of owning spec/);
  f.put('docs/UI_USER_FLOWS.md', doc() + doc('02')); rmSync(path.join(f.root, specPath('UF-01')));
  assert.match(missing(f.check()), /removal of the UF section/);
});
test('implementation, styles, examples, dependencies and browser configuration select all', t => {
  const f = fixture(t);
  for (const file of ['ui/src/editor.tsx', 'ui/src/style.css', 'ui/playground.ts', 'workflows/example.yml', 'ui/package-lock.json', 'ui/vite.config.ts']) {
    f.put(file, 'changed'); assert.deepEqual(f.check().selected, ['UF-01', 'UF-02']); rmSync(path.join(f.root, file));
  }
});
test('generated artifacts do not affect selection or synchronization', t => {
  const f = fixture(t); f.put('ui/dist/output.js', 'generated'); f.put('ui/test-results/output.txt', 'report'); f.put('ui/playwright-report/index.html', 'report');
  assert.deepEqual(f.check().selected, []); assert.deepEqual(f.check().findings, []);
});
test('diverged branch comparison uses the merge base', t => {
  const f = fixture(t); git(f.root, 'checkout', '-qb', 'feature'); f.put('notes.txt', 'feature'); git(f.root, 'add', '.'); git(f.root, 'commit', '-qm', 'feature');
  git(f.root, 'checkout', 'main'); f.put('upstream.txt', 'upstream'); git(f.root, 'add', '.'); git(f.root, 'commit', '-qm', 'upstream'); git(f.root, 'checkout', 'feature');
  assert.equal(baseline(f.root, { base: 'main' }), f.base);
});
test('missing baseline explains fetch, explicit override and rerun commands', t => {
  const f = fixture(t); assert.throws(() => baseline(f.root, { base: 'origin/unavailable' }), /git fetch origin unavailable:refs\/remotes\/origin\/unavailable[\s\S]*make ui-pr-check BASE=origin\/unavailable/);
});
test('CI uses actual PR base SHA and push runs have no pairing baseline', t => {
  const f = fixture(t); f.put('event.json', JSON.stringify({ pull_request: { base: { sha: f.base } } }));
  const env = { GITHUB_EVENT_PATH: path.join(f.root, 'event.json') };
  assert.equal(baseline(f.root, { ci: true, base: 'missing', env }), f.base);
  f.put('event.json', '{}'); assert.equal(baseline(f.root, { ci: true, env }), null);
});
for (const kind of ['skip', 'fixme', 'fail', 'only']) test(`discovery rejects ${kind} cases`, t => {
  const f = fixture(t); f.put(specPath('UF-01'), spec.replace("test('flow'", `test.${kind}('flow'`));
  assert.match(missing(f.check()), /unconditional active tests/);
  assert.ok(checkDiscovered(f.check().sections, discovery(f.root)).length);
});
test('discovery rejects a spec with no discovered cases', t => {
  const f = fixture(t); f.put(specPath('UF-01'), 'export const empty = true;');
  assert.match(checkDiscovered(f.check().sections, discovery(f.root)).map(f => f.missing).join(), /discovered active/);
});
for (const [name, body, status] of [
  ['empty case', '', 1], ['runtime skip', 'test.skip(true, "skip");', 1],
  ['expected failure', 'test.fail(); expect(1).toBe(2);', 1], ['passing assertion', 'expect(1).toBe(1);', 0],
]) test(`runtime reporter enforces ${name}`, t => {
  const f = fixture(t); f.put(specPath('UF-01'), `import { test, expect } from '@playwright/test'; test('runtime', async () => { ${body} });`);
  const run = spawnSync(process.execPath, ['node_modules/@playwright/test/cli.js', 'test', 'uf-01', '--workers=2', '--retries=0'], { cwd: path.join(f.root, 'ui'), encoding: 'utf8' });
  assert.equal(run.status, status, run.stdout + run.stderr);
  if (status) assert.match(run.stderr, /must pass and execute an assertion/);
});
test('failure output, annotations and summary carry the same repair instructions', t => {
  const f = fixture(t); f.put(specPath('UF-01'), spec.replace('toBe(1)', 'toEqual(1)'));
  const result = f.check(); const summary = path.join(f.root, 'summary');
  const errors = [], logs = []; const stderr = process.stderr.write, log = console.log;
  try { process.stderr.write = text => { errors.push(text); return true; }; console.log = text => logs.push(text); report(result.findings, 'origin/main', { GITHUB_ACTIONS: 'true', GITHUB_STEP_SUMMARY: summary }); }
  finally { process.stderr.write = stderr; console.log = log; }
  assert.match(errors.join(''), /UF-01 — Flow 01[\s\S]*Changed:[\s\S]*Missing:[\s\S]*Coverage note[\s\S]*make ui-pr-check BASE=origin\/main/);
  assert.match(logs.join(''), /::error file=.*UF-01/);
  assert.ok(readFileSync(summary, 'utf8').includes(errors.join('').trim()));
});

test('stable identifiers cannot be renumbered by a paired remove/add', t => {
  const f = fixture(t); f.put('docs/UI_USER_FLOWS.md', doc().replaceAll('UF-01', 'UF-03').replaceAll('uf-01', 'uf-03') + doc('02'));
  rmSync(path.join(f.root, specPath('UF-01'))); f.put(specPath('UF-03'), spec);
  assert.match(missing(f.check()), /stable identifier UF-01/);
});
test('transitive helpers select only their importing flows', t => {
  const f = fixture(t); f.put('ui/test/browser/a.ts', "export { value } from './b';"); f.put('ui/test/browser/b.ts', 'export const value = 1;');
  f.put(specPath('UF-01'), "import { value } from '../a';\n" + spec);
  git(f.root, 'add', '.'); git(f.root, 'commit', '-qm', 'imports'); const base = git(f.root, 'rev-parse', 'HEAD');
  f.put('ui/test/browser/b.ts', 'export const value = 2;');
  const result = analyze(revision(f.root), revision(f.root, base));
  assert.deepEqual(result.selected, ['UF-01']); assert.match(missing(result), /UF-01: substantive update/);
});
test('missing project coverage is rejected by discovery validation', t => {
  const f = fixture(t); const result = f.check(); const found = discovery(f.root); found.projects.push('firefox');
  assert.equal(checkDiscovered(result.sections, found).length, 2);
});
test('nested and differently named flow specs are orphaned', t => {
  const f = fixture(t); f.put('ui/test/browser/flows/nested/uf-01.spec.ts', spec); f.put('ui/test/browser/flows/extra.test.ts', spec);
  assert.equal(f.check().findings.filter(f => f.missing === 'owning UF section').length, 2);
});
test('hook assertions alone cannot cover an empty case', t => {
  const f = fixture(t); f.put(specPath('UF-01'), "import { test, expect } from '@playwright/test'; test.beforeEach(() => { expect(1).toBe(1); }); test('empty', async () => {});");
  const run = spawnSync(process.execPath, ['node_modules/@playwright/test/cli.js', 'test', 'uf-01'], { cwd: path.join(f.root, 'ui'), encoding: 'utf8' });
  assert.equal(run.status, 1); assert.match(run.stderr, /must pass and execute an assertion/);
});

test('unrelated command completes without executing browser tests', t => {
  const f = fixture(t); f.put('notes.txt', 'unrelated');
  const run = spawnSync(process.execPath, [path.join(moduleDir, 'flows-check.mjs'), `--base=${f.base}`, '--run'], { cwd: f.root, encoding: 'utf8' });
  assert.equal(run.status, 0, run.stderr); assert.match(run.stdout, /No UF browser run needed/); assert.doesNotMatch(run.stdout, /Running \d+ tests/);
});
test('test marker strings and comments are not skipped test declarations', t => {
  const f = fixture(t); f.put(specPath('UF-01'), spec.replace('expect(1)', 'expect("test.skip")') + '// test.only\n');
  assert.ok(!f.check().findings.some(f => f.missing === 'unconditional active tests'));
});
test('Markdown formatting and TypeScript comments select no flows', t => {
  const f = fixture(t); f.put('docs/UI_USER_FLOWS.md', doc().replace('Inspect', '**Inspect**') + '\n' + doc('02'));
  f.put(specPath('UF-01'), '// explanatory comment\n' + spec.replace('toBe(1)', 'toBe( 1 )'));
  assert.deepEqual(f.check().findings, []); assert.deepEqual(f.check().selected, []);
});
test('missing locked dependencies produces setup guidance', t => {
  const f = fixture(t); rmSync(path.join(f.root, 'ui/node_modules'));
  f.put('ui/scripts/flows-check.mjs', readFileSync(path.join(moduleDir, 'flows-check.mjs'), 'utf8'));
  const run = spawnSync(process.execPath, [path.join(f.root, 'ui/scripts/flows-check.mjs')], { cwd: f.root, encoding: 'utf8' });
  assert.equal(run.status, 1); assert.match(run.stderr, /Run: make ui-install/);
});

test('nested documentation headings stay inside their owning UF section', t => {
  const f = fixture(t); f.put('docs/UI_USER_FLOWS.md', doc('01', 'Inspect a workflow.\n#### Coverage\nOld scenario.') + doc('02'));
  git(f.root, 'add', '.'); git(f.root, 'commit', '-qm', 'nested coverage'); const base = git(f.root, 'rev-parse', 'HEAD');
  f.put('docs/UI_USER_FLOWS.md', doc('01', 'Inspect a workflow.\n#### Coverage\nNew recovery scenario.') + doc('02'));
  assert.match(missing(analyze(revision(f.root), revision(f.root, base))), /UF-01: substantive change/);
});
