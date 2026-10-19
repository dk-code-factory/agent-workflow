import { execFileSync, spawnSync } from 'node:child_process';
import { readFileSync, appendFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
let ts;
try { ts = (await import('typescript')).default; } catch {
  console.error('UI flow checker needs the locked Node dependencies. Run: make ui-install');
  process.exit(1);
}

export const documentPath = 'docs/UI_USER_FLOWS.md';
export const specPath = id => `ui/test/browser/flows/uf-${id.slice(3)}.spec.ts`;
const fixtures = ['ui/test/browser.html', 'ui/test/browser.tsx'];
const ignored = /(^|\/)(node_modules|dist|test-results|playwright-report|\.git)(\/|$)|\.tgz$/;
const syntaxFile = /\.[cm]?[jt]sx?$/;
const command = base => `make ui-pr-check BASE=${base ?? '<PR target>'}`;
const git = (root, args, input) => execFileSync('git', ['-C', root, ...args], { encoding: 'utf8', input, stdio: ['pipe', 'pipe', 'pipe'], maxBuffer: 32 * 1024 * 1024 });

export function syntax(source, name = 'file.ts') {
  const file = ts.createSourceFile(name, source, ts.ScriptTarget.Latest, true, name.endsWith('x') ? ts.ScriptKind.TSX : ts.ScriptKind.TS);
  function node(n) {
    const children = []; ts.forEachChild(n, child => { children.push(node(child)); });
    return [n.kind, (ts.isIdentifier(n) || ts.isPrivateIdentifier(n) || ts.isLiteralExpression(n) || ts.isTemplateLiteralToken(n) || ts.isJsxText(n)) ? n.text : null, ts.isVariableDeclarationList(n) ? n.flags & (ts.NodeFlags.Let | ts.NodeFlags.Const) : null, n.isTypeOnly ?? null, children];
  }
  return JSON.stringify(node(file));
}
export function prose(text) {
  return text.replace(/<!--[\s\S]*?-->/g, '').replace(/^.*\*\*UI tests\*\*.*\n?/gm, '').replace(/\[UI tests\]\([^)]*\)/g, '').replace(/\[([^\]]+)\]\([^)]*\)/g, '$1').replace(/^\s*(?:#{1,6}\s+|[-*+]\s+|\d+\.\s+)/gm, '').replace(/`/g, '').replace(/(?<!\w)[*_]+|[*_]+(?!\w)/g, '').replace(/\s+/g, ' ').trim();
}
export function parseSections(source) {
  const sections = new Map(); const errors = [];
  const headings = [...source.matchAll(/^#{1,6} .*$/gm)];
  for (let index = 0; index < headings.length; index++) {
    const heading = headings[index];
    if (!/^#+\s+UF/i.test(heading[0])) continue;
    const match = /^### (UF-\d{2}) — (\S.*)$/.exec(heading[0]);
    if (!match || match[1] === 'UF-00') { errors.push({ id: 'UF-??', title: heading[0], file: documentPath, missing: 'valid UF heading', action: 'Use ### UF-NN — Title with a stable two-digit ID.' }); continue; }
    const [, id, title] = match;
    if (sections.has(id)) errors.push({ id, title, file: documentPath, missing: 'unique UF ID', action: 'Remove the duplicate ID; preserve existing flow identifiers.' });
    sections.set(id, { id, title, text: source.slice(heading.index, headings.slice(index + 1).find(next => /^#{1,3} /.test(next[0]))?.index ?? source.length), line: source.slice(0, heading.index).split('\n').length });
  }
  if (!sections.size) errors.push({ id: 'UF-??', title: 'UI user flows', file: documentPath, missing: 'UF sections', action: 'Restore the user-flow document and its stable UF headings.' });
  return { sections, errors };
}
export function revision(root, ref) {
  const files = new Set((ref ? git(root, ['ls-tree', '-r', '--name-only', '-z', ref]) : git(root, ['ls-files', '--cached', '--others', '--exclude-standard', '-z'])).split('\0').filter(f => f && !ignored.test(f)));
  const cache = new Map();
  return { files, read(file) {
    if (!cache.has(file)) {
      let value = null;
      if (files.has(file)) { try { value = ref ? git(root, ['show', `${ref}:${file}`]) : readFileSync(path.join(root, file), 'utf8'); } catch { /* deleted working file */ } }
      cache.set(file, value);
    }
    return cache.get(file);
  } };
}
function dependencies(view, file, found = new Set()) {
  if (found.has(file)) return found;
  found.add(file); const source = view.read(file); if (source === null || !syntaxFile.test(file)) return found;
  const ast = ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true);
  function visit(node) {
    let value;
    if ((ts.isImportDeclaration(node) || ts.isExportDeclaration(node)) && node.moduleSpecifier && ts.isStringLiteral(node.moduleSpecifier)) value = node.moduleSpecifier.text;
    if (ts.isCallExpression(node) && (node.expression.kind === ts.SyntaxKind.ImportKeyword || node.expression.getText(ast) === 'require') && node.arguments[0] && ts.isStringLiteral(node.arguments[0])) value = node.arguments[0].text;
    if (value?.startsWith('.')) {
      const stem = path.posix.normalize(path.posix.join(path.posix.dirname(file), value.split(/[?#]/, 1)[0]));
      const candidates = [stem, ...['.ts', '.tsx', '.mts', '.cts', '.js', '.jsx', '.mjs', '.cjs', '/index.ts', '/index.tsx'].map(ext => stem + ext), stem.replace(/\.js$/, '.ts')];
      const resolved = candidates.find(candidate => view.read(candidate) !== null);
      if (resolved) dependencies(view, resolved, found);
      else for (const candidate of candidates) found.add(candidate); // keep deleted inputs visible
    }
    ts.forEachChild(node, visit);
  }
  visit(ast);
  // The browser harness is loaded by HTML rather than a spec import.
  if (file.startsWith('ui/test/browser/')) for (const fixture of fixtures) dependencies(view, fixture, found);
  return found;
}
const normalized = (file, value) => value === null ? null : syntaxFile.test(file) ? syntax(value, file) : value.replace(/\s+/g, ' ').trim();
function forbiddenDeclarations(source, file) {
  const ast = ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true);
  let forbidden = false;
  function visit(node) {
    if (ts.isCallExpression(node)) {
      let expression = node.expression;
      let restricted = false;
      while (ts.isPropertyAccessExpression(expression) || ts.isElementAccessExpression(expression)) {
        const name = ts.isPropertyAccessExpression(expression) ? expression.name.text : expression.argumentExpression?.text;
        restricted ||= ['skip', 'fixme', 'only', 'fail'].includes(name);
        expression = expression.expression;
      }
      if (restricted && ts.isIdentifier(expression) && ['test', 'describe'].includes(expression.text)) forbidden = true;
    }
    ts.forEachChild(node, visit);
  }
  visit(ast); return forbidden;
}
export function analyze(current, previous) {
  const parsed = parseSections(current.read(documentPath) ?? ''); const findings = [...parsed.errors];
  const old = previous ? parseSections(previous.read(documentPath) ?? '').sections : new Map();
  const affected = new Set(); const shared = new Map();
  const changedCache = new Map();
  const changed = file => {
    if (!changedCache.has(file)) changedCache.set(file, !!previous && normalized(file, current.read(file)) !== normalized(file, previous.read(file)));
    return changedCache.get(file);
  };
  for (const [id, section] of parsed.sections) {
    const file = specPath(id); const expected = `../${file}`;
    const links = [...section.text.matchAll(/\[UI tests\]\(([^)]+)\)/g)];
    if (links.length !== 1 || links[0][1] !== expected) findings.push({ ...section, file: documentPath, missing: 'correct UI tests link', action: `Add [UI tests](${expected}) inside this UF section.` });
    if (current.read(file) === null) findings.push({ ...section, file, missing: 'owning Playwright spec', action: `Create ${file} with active tests exercising this flow through visible controls.` });
  }
  for (const file of current.files) if (/^ui\/test\/browser\/flows\/.*\.(?:spec|test)\.[cm]?[jt]sx?$/.test(file) && current.read(file) !== null) {
    const match = /\/uf-(\d{2})\.spec\.ts$/.exec(file); const id = match ? `UF-${match[1]}` : 'UF-??';
    if (!parsed.sections.has(id) || file !== specPath(id)) findings.push({ id, title: 'Orphan flow spec', file, missing: 'owning UF section', action: `Add the corresponding ${id} section and coverage link, or remove the orphan spec.` });
    const source = current.read(file);
    if (forbiddenDeclarations(source, file)) findings.push({ id, title: parsed.sections.get(id)?.title ?? 'Flow tests', file, missing: 'unconditional active tests', action: 'Remove skip, fixme, only and expected-failure declarations; every UF case must pass.' });
  }
  if (previous) for (const id of new Set([...old.keys(), ...parsed.sections.keys()])) {
    const section = parsed.sections.get(id) ?? old.get(id); const file = specPath(id);
    const docChanged = prose(parsed.sections.get(id)?.text ?? '') !== prose(old.get(id)?.text ?? '');
    const testChanged = changed(file);
    const deps = new Set([...dependencies(current, file), ...dependencies(previous, file)]);
    const helpers = [...deps].filter(dep => dep !== file && !dep.startsWith('ui/src/') && !/^ui\/[^/]+$/.test(dep) && changed(dep)).sort();
    const added = !old.has(id); const removed = !parsed.sections.has(id);
    if (added) for (const [priorID, prior] of old) {
      if (!parsed.sections.has(priorID) && prior.title === section.title) findings.push({ ...section, file: documentPath, missing: `stable identifier ${priorID}`, action: `Keep ${priorID} for this existing flow instead of renumbering it to ${id}.` });
    }
    if (docChanged || testChanged || helpers.length) affected.add(id);
    if (removed && current.read(file) !== null) findings.push({ ...section, file: documentPath, missing: 'removal of owning spec', action: `Remove ${file} together with the removed UF section. Do not renumber remaining IDs.` });
    else if (added && current.read(file) === null) { /* structural finding above */ }
    else if (!removed && (testChanged || helpers.length) && !docChanged) {
      const finding = { ...section, file: helpers.length ? helpers.join(', ') : file, missing: `substantive update to the ${id} section`, action: 'Update the flow description to match the test. If behavior is unchanged, update its Coverage note to explain the revised assertions or recovery scenario.' };
      if (helpers.length) { const key = helpers.join(', '); if (!shared.has(key)) shared.set(key, []); shared.get(key).push(finding); }
      else findings.push(finding);
    } else if (!removed && docChanged && !testChanged && !helpers.length) findings.push({ ...section, file: `${documentPath} (${id} section)`, missing: `substantive change to ${file}`, action: 'Update the owning spec to assert the revised behavior. Formatting and test comments do not satisfy pairing.' });
    if (!added && !removed && current.read(file) === null && previous.read(file) !== null) findings.push({ ...section, file, missing: 'removal of the UF section', action: 'Restore the active spec or remove both the spec and its UF section.' });
  }
  for (const [files, group] of shared) {
    findings.push(...group.map(f => ({ ...f, group: `Shared test dependency changed: ${files}; affects ${group.map(s => s.id).join(', ')}` })));
  }
  const allFiles = new Set([...current.files, ...(previous?.files ?? [])]);
  const full = !previous || [...allFiles].some(file => !ignored.test(file) && /^(ui\/(src\/|playground\.|index\.html|package(?:-lock)?\.json|[^/]*config\.|scripts\/(flows-check|flow-reporter|flows-check\.test))|Makefile$|workflows\/|testdata\/ui-compat\/|\.github\/workflows\/test\.yml)/.test(file) && changed(file));
  return { findings, sections: parsed.sections, selected: [...parsed.sections.keys()].filter(id => full || affected.has(id)).sort(), full };
}
export function baseline(root, { base, ci = false, env = process.env } = {}) {
  if (ci) {
    const event = env.GITHUB_EVENT_PATH ? JSON.parse(readFileSync(env.GITHUB_EVENT_PATH, 'utf8')) : {};
    if (!event.pull_request) return null;
    base = event.pull_request.base.sha;
    if (!base) throw new Error('PR event is missing pull_request.base.sha. Supply BASE=<PR base SHA>.');
  }
  base ||= 'origin/main';
  try { return git(root, ['merge-base', base, 'HEAD']).trim(); }
  catch {
    const shallow = git(root, ['rev-parse', '--is-shallow-repository']).trim() === 'true';
    throw new Error(`Cannot resolve merge base for ${base}. ${shallow ? 'Run: git fetch --unshallow origin\n' : ''}Run: git fetch origin ${base.startsWith('origin/') ? `${base.slice(7)}:refs/remotes/${base}` : base}\nThen rerun: ${command(base)}\nOr supply: make ui-pr-check BASE=<available PR target>`);
  }
}
export function discovery(root) {
  const run = spawnSync(process.execPath, ['node_modules/@playwright/test/cli.js', 'test', 'flows', '--list', '--forbid-only', '--reporter=json'], { cwd: path.join(root, 'ui'), encoding: 'utf8', maxBuffer: 16 * 1024 * 1024 });
  let data; try { data = JSON.parse(run.stdout); } catch { throw new Error(`Playwright discovery failed. Run: make ui-install\n${run.stderr ?? run.error?.message ?? ''}`); }
  const specs = []; const walk = suite => { specs.push(...(suite.specs ?? [])); (suite.suites ?? []).forEach(walk); }; (data.suites ?? []).forEach(walk);
  return { specs, projects: data.config?.projects?.map(project => project.name) ?? [], errors: [...(data.errors ?? []).map(e => e.message), ...(run.status && !data.errors?.length ? [run.stderr || 'Playwright discovery failed'] : [])] };
}
export function checkDiscovered(sections, discovered) {
  const findings = discovered.errors.map(message => ({ id: 'UF-??', title: 'Playwright discovery', file: 'ui/test/browser/flows', missing: 'valid active test discovery', action: message }));
  for (const [id, section] of sections) {
    const cases = discovered.specs.filter(spec => spec.file.replaceAll('\\', '/').endsWith(`flows/uf-${id.slice(3)}.spec.ts`));
    if (!cases.length || discovered.projects?.some(project => !cases.some(spec => spec.tests.some(t => t.projectName === project))) || cases.some(spec => !spec.tests.length || spec.tests.some(t => t.expectedStatus !== 'passed' || t.annotations?.some(a => ['skip', 'fixme', 'fail'].includes(a.type))))) findings.push({ ...section, file: specPath(id), missing: 'discovered active passing cases', action: 'Add active Playwright tests. Remove skip, fixme, focused and expected-failure cases; run discovery again.' });
  }
  return findings;
}
export function report(findings, base, env = process.env) {
  const render = f => `${f.id} — ${f.title}\nChanged: ${f.file}\nMissing: ${f.missing}\n\n${f.action}\n\nRerun: ${command(base)}`;
  const groups = new Map();
  for (const finding of findings) {
    const key = finding.group ?? '';
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(finding);
  }
  const blocks = [...groups].flatMap(([group, items]) => [
    ...(group ? [group] : []), ...items.map(render),
  ]);
  if (blocks.length) process.stderr.write(blocks.join('\n\n') + '\n');
  if (env.GITHUB_ACTIONS) for (const f of findings) {
    const escape = text => String(text).replaceAll('%', '%25').replaceAll('\r', '%0D').replaceAll('\n', '%0A').replaceAll(',', '%2C').replaceAll(':', '%3A');
    console.log(`::error file=${escape(f.file.split(',')[0].replace(/ \(UF-\d{2} section\)$/, ''))},line=${f.file.startsWith(documentPath) ? f.line ?? 1 : f.testLine ?? 1},title=${escape(f.id + ' — ' + f.title)}::${escape(`${f.group ? f.group + '. ' : ''}Missing: ${f.missing}. ${f.action} Rerun: ${command(base)}`)}`);
  }
  if (env.GITHUB_STEP_SUMMARY) appendFileSync(env.GITHUB_STEP_SUMMARY, findings.length ? blocks.map(b => `\n\n\`\`\`text\n${b}\n\`\`\``).join('') : '\nUI flow structure and synchronization passed.\n');
}
export function main(args = process.argv.slice(2)) {
  const root = git(process.cwd(), ['rev-parse', '--show-toplevel']).trim();
  const base = args.find(arg => arg.startsWith('--base='))?.slice(7) || process.env.BASE;
  const ref = baseline(root, { base, ci: args.includes('--ci') });
  const displayBase = base || (args.includes('--ci') ? ref : 'origin/main');
  const result = analyze(revision(root), ref ? revision(root, ref) : null);
  result.findings.push(...checkDiscovered(result.sections, discovery(root)));
  report(result.findings, displayBase);
  if (result.findings.length) return 1;
  console.log(`UI flows: ${result.sections.size} mappings valid; ${result.full ? 'all flows selected' : `${result.selected.length} affected flows`}.`);
  if (!args.includes('--run')) return 0;
  if (!result.selected.length) { console.log('No UF browser run needed for these changes.'); return 0; }
  console.log('Run setup if needed: make ui-install && cd ui && npx playwright install --with-deps chromium');
  const run = spawnSync(process.execPath, ['node_modules/@playwright/test/cli.js', 'test', ...result.selected.map(id => `flows/uf-${id.slice(3)}.spec.ts`), '--project=chromium', '--workers=2', '--retries=0'], { cwd: path.join(root, 'ui'), stdio: 'inherit', env: { ...process.env, BASE: displayBase ?? 'origin/main' } });
  return run.status ?? 1;
}
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { process.exitCode = main(); } catch (error) { const f = { id: 'UF-??', title: 'UI flow gate setup', file: documentPath, missing: 'available baseline or tooling', action: error.message }; report([f], process.env.BASE); process.exitCode = 1; }
}
