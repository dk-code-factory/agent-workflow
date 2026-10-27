import { describe, expect, it, vi } from 'vitest';
import { WorkflowDocument } from '../src/document';
import { parseWorkflow } from '../src/validation';
import { references } from '../src/expressions';
const source = `# preserved header\nname: Example # inline\njobs:\n  build:\n    outputs:\n      result: \${{ steps.step_1.outputs.text }}\n    steps:\n      # prompt comment\n      - uses: agent/claude\n        with:\n          prompt: |\n            Hello\n            world \${{ inputs.task }}\n      - argv: [echo, done]\n  test:\n    needs: build\n    steps:\n      - argv: [echo, '\${{ needs.build.outputs.result }}']\n`;
describe('document edits', () => {
  it('keeps agent parameters, YAML comments and block prompts across provider changes', () => {
    const doc = new WorkflowDocument({ source });
    doc.setStepKind('build', 0, 'agent/codex');
    expect(doc.getSnapshot().workflow!.jobs.build.steps![0].with).toEqual(new WorkflowDocument({ source }).getSnapshot().workflow!.jobs.build.steps![0].with);
    expect(doc.getSnapshot().source).toContain('# prompt comment');
    expect(doc.getSnapshot().source).toContain('prompt: |');
    expect(doc.getSnapshot().source).toContain('# preserved header');
    doc.undo(); expect(doc.getSnapshot().source).toBe(source);
    doc.redo(); expect(doc.getSnapshot().workflow!.jobs.build.steps![0].uses).toBe('agent/codex');
  });
  it('retains common controller parameters and initializes null action parameters', () => {
    const doc = new WorkflowDocument({ source: 'jobs: {build: {steps: [{uses: git/commit, with: {path: /work, message: Keep}}, {uses: agent/claude, with: null}]}}' });
    doc.setStepKind('build', 0, 'github/pr');
    expect(doc.getSnapshot().workflow!.jobs.build.steps![0]).toEqual({ uses: 'github/pr', with: { path: '/work' } });
    doc.setStepKind('build', 1, 'agent/codex');
    expect(doc.getSnapshot().workflow!.jobs.build.steps![1]).toEqual({ uses: 'agent/codex', with: { prompt: '' } });
  });
  it('creates an action parameter mapping when converting a command', () => {
    const original = 'jobs: {build: {steps: [{id: run, argv: [echo, before], timeout: 2m}]}}';
    const doc = new WorkflowDocument({ source: original });
    doc.setStepKind('build', 0, 'agent/claude');
    expect(doc.getSnapshot().workflow!.jobs.build.steps![0]).toEqual({ id: 'run', uses: 'agent/claude', with: { prompt: '' } });
    doc.undo(); expect(doc.getSnapshot().source).toBe(original);
  });
  it.each(['argv', 'shell'])('initializes a usable %s command when imported command fields are empty', kind => {
    const original = "jobs: {build: {steps: [{id: run, uses: agent/claude, with: {prompt: Hello}, argv: [], run: ''}]}}";
    const doc = new WorkflowDocument({ source: original });
    expect(doc.getSnapshot().diagnostics).toEqual([]);
    doc.setStepKind('build', 0, kind);
    expect(doc.getSnapshot().workflow!.jobs.build.steps![0]).toEqual(kind === 'argv'
      ? { id: 'run', argv: ['echo', 'Hello'] }
      : { id: 'run', shell: 'sh', run: 'echo Hello' });
    expect(doc.getSnapshot().diagnostics).toEqual([]);
    doc.undo(); expect(doc.getSnapshot().source).toBe(original);
  });
  it('exports untouched source byte for byte, including CRLF', () => {
    const text = source.replaceAll('\n', '\r\n');
    expect(new WorkflowDocument({ source: text }).getSnapshot().source).toBe(text);
  });
  it('preserves comments, block prompts, ordering, and imported fields', () => {
    const doc = new WorkflowDocument({ source: source + 'imported: keep\n' });
    doc.setField(['name'], 'Renamed');
    const text = doc.getSnapshot().source;
    expect(text).toContain('# preserved header'); expect(text).toContain('# inline'); expect(text).toContain('# prompt comment'); expect(text).toContain('prompt: |'); expect(text).toContain('imported: keep');
    expect(text.indexOf('build:')).toBeLessThan(text.indexOf('test:'));
  });
  it('renames jobs atomically in dependencies and recognized expression paths', () => {
    const doc = new WorkflowDocument({ source }); doc.renameJob('build', 'compile');
    expect(doc.getSnapshot().workflow!.jobs.test.needs).toEqual(['compile']);
    expect(doc.getSnapshot().source).toContain('needs.compile.outputs.result');
    doc.undo(); expect(doc.getSnapshot().source).toBe(source);
  });
  it('skips quoted literals and plain prose in expression rewriting', () => {
    const text = `plain needs.build.outputs.x \${{ 'needs.build.outputs.x' == needs.build.outputs.x }}`;
    const doc = new WorkflowDocument({ source }); doc.setField(['jobs', 'build', 'steps', 0, 'with', 'prompt'], text); doc.renameJob('build', 'compile');
    expect(doc.getSnapshot().workflow!.jobs.compile.steps![0].with!.prompt).toBe(`plain needs.build.outputs.x \${{ 'needs.build.outputs.x' == needs.compile.outputs.x }}`);
    expect(references(text)).toHaveLength(1);
  });
  it('materializes implicit IDs before reorder and deletion', () => {
    const doc = new WorkflowDocument({ source }); doc.moveStep('build', 0, 1);
    expect(doc.getSnapshot().workflow!.jobs.build.steps!.map(s => s.id)).toEqual(['step_2', 'step_1']);
    expect(doc.getSnapshot().diagnostics.filter(d => d.code === 'expression-reference')).toEqual([]);
    doc.deleteStep('build', 0); expect(doc.getSnapshot().workflow!.jobs.build.steps![0].id).toBe('step_1');
  });
  it('renames only local step references', () => {
    const doc = new WorkflowDocument({ source }); doc.renameStep('build', 0, 'ask');
    expect(doc.getSnapshot().source).toContain('steps.ask.outputs.text');
    expect(doc.getSnapshot().source).not.toContain('steps.step_1.outputs.text');
  });
  it('renames bare conditions and repeat expressions without rewriting prose or quoted operands', () => {
    const doc = new WorkflowDocument({ source });
    doc.setField(['jobs', 'test', 'if'], "needs.build.status == 'success' and 'needs.build.status' != ''");
    doc.setField(['jobs', 'build', 'steps', 1, 'if'], 'not steps.step_1.outputs.failed');
    doc.setField(['jobs', 'build', 'repeat'], { 'max-attempts': 3, until: 'steps.step_1.outputs.done == true' });
    doc.setField(['jobs', 'build', 'steps', 0, 'with', 'prompt'], 'plain steps.step_1.outputs.done');
    doc.renameJob('build', 'compile'); doc.renameStep('compile', 0, 'ask');
    const workflow = doc.getSnapshot().workflow!;
    expect(workflow.jobs.test.if).toBe("needs.compile.status == 'success' and 'needs.build.status' != ''");
    expect(workflow.jobs.compile.steps![1].if).toBe('not steps.ask.outputs.failed');
    expect(workflow.jobs.compile.repeat!.until).toBe('steps.ask.outputs.done == true');
    expect(workflow.jobs.compile.steps![0].with!.prompt).toBe('plain steps.step_1.outputs.done');
    expect(doc.getSnapshot().diagnostics.filter(d => d.code === 'expression-reference')).toEqual([]);
  });
  it('includes bare condition and whole-object references in deletion impacts and diagnostics', () => {
    const doc = new WorkflowDocument({ source });
    doc.setField(['jobs', 'test', 'if'], 'needs.build');
    doc.setField(['jobs', 'build', 'steps', 1, 'if'], 'steps.step_1');
    doc.setField(['jobs', 'build', 'repeat'], { 'max-attempts': 3, until: '${{ steps.step_1.outputs.done }}' });
    expect(doc.deletionImpact('build')).toContainEqual(['jobs', 'test', 'if']);
    expect(doc.deletionImpact('build', 0)).toEqual(expect.arrayContaining([
      ['jobs', 'build', 'steps', 1, 'if'], ['jobs', 'build', 'repeat', 'until'],
    ]));
    doc.deleteStep('build', 0, true);
    expect(doc.getSnapshot().diagnostics.filter(d => d.code === 'expression-reference').map(d => d.path)).toContainEqual(['jobs', 'build', 'steps', 0, 'if']);
    doc.deleteJob('build', true);
    expect(doc.getSnapshot().diagnostics.filter(d => d.code === 'expression-reference').map(d => d.path)).toContainEqual(['jobs', 'test', 'if']);
  });
  it('renames callable entries and job outputs atomically while preserving comments and order', () => {
    const doc = new WorkflowDocument({ source: `# callable\non:\n  workflow_call:\n    inputs:\n      task: # input comment\n        type: string\n      other: {type: string}\n    secrets:\n      token: {required: true}\n    outputs:\n      result: {value: '\${{ jobs.build.outputs.result }}'}\njobs:\n  build:\n    outputs:\n      result: '\${{ inputs.task }}' # output comment\n    steps:\n      - argv: [echo, '\${{ secrets.token }}']\n  test:\n    needs: build\n    if: needs.build.outputs.result != ''\n    steps:\n      - argv: [echo, 'plain inputs.task and secrets.token']\n` });
    const original = doc.getSnapshot().source;
    doc.renameEntry(['on', 'workflow_call', 'inputs', 'task'], 'request');
    doc.renameEntry(['on', 'workflow_call', 'secrets', 'token'], 'api_key');
    doc.renameEntry(['jobs', 'build', 'outputs', 'result'], 'answer');
    doc.renameEntry(['on', 'workflow_call', 'outputs', 'result'], 'response');
    const snapshot = doc.getSnapshot();
    expect(snapshot.source).toContain('# input comment'); expect(snapshot.source).toContain('# output comment');
    expect(snapshot.source.indexOf('request:')).toBeLessThan(snapshot.source.indexOf('other:'));
    expect(snapshot.workflow!.jobs.test.if).toBe("needs.build.outputs.answer != ''");
    expect(snapshot.workflow!.jobs.build.outputs!.answer).toBe('${{ inputs.request }}');
    expect(snapshot.source).toContain('jobs.build.outputs.answer'); expect(snapshot.source).toContain('secrets.api_key');
    expect(snapshot.source).toContain('plain inputs.task and secrets.token');
    expect(snapshot.diagnostics.filter(d => d.code === 'expression-reference')).toEqual([]);
    doc.undo(); doc.undo(); doc.undo(); doc.undo(); expect(doc.getSnapshot().source).toBe(original);
  });
  it('checks entry names and confirms referenced deletion with dangling interface/output diagnostics', () => {
    const doc = new WorkflowDocument({ source });
    doc.setField(['on', 'workflow_call'], { inputs: { task: { type: 'string' }, other: { type: 'string' } }, secrets: { token: { required: true } }, outputs: { result: { value: '${{ jobs.build.outputs.result }}' } } });
    doc.setField(['jobs', 'test', 'if'], "needs.build.outputs.result != '' and secrets.token != ''");
    expect(() => doc.renameEntry(['on', 'workflow_call', 'inputs', 'task'], 'other')).toThrow('unique');
    expect(() => doc.renameEntry(['on', 'workflow_call', 'inputs', 'task'], 'bad id')).toThrow('valid');
    expect(() => doc.deleteEntry(['on', 'workflow_call', 'inputs', 'task'])).toThrow('confirmation');
    expect(doc.entryDeletionImpact(['jobs', 'build', 'outputs', 'result'])).toContainEqual(['jobs', 'test', 'if']);
    doc.deleteEntry(['on', 'workflow_call', 'inputs', 'task'], true);
    doc.deleteEntry(['on', 'workflow_call', 'secrets', 'token'], true);
    doc.deleteEntry(['jobs', 'build', 'outputs', 'result'], true);
    const messages = doc.getSnapshot().diagnostics.filter(d => d.code === 'expression-reference').map(d => d.message);
    expect(messages).toEqual(expect.arrayContaining(['Unknown inputs reference "task"', 'Secret "token" is not declared in the callable interface; the host must supply it', 'Unknown output "result" of job "build"']));
    expect(doc.getSnapshot().diagnostics.find(d => d.message.startsWith('Secret "token"'))).toMatchObject({ source: 'host', severity: 'warning' });
    doc.deleteEntry(['on', 'workflow_call', 'outputs', 'result']);
    expect(() => doc.renameEntry(['jobs', 'build', 'steps', 0], 'new')).toThrow('entry');
  });
  it('keeps undeclared manual input/secret names and reusable outputs host-dependent', () => {
    const doc = new WorkflowDocument({ source: `jobs:\n  call:\n    uses: workflows/child.yml\n    with: {task: '\${{ inputs.task }}'}\n    secrets: {token: '\${{ secrets.token }}'}\n  test:\n    needs: call\n    steps: [{argv: [echo, '\${{ needs.call.outputs.dynamic }}']}]\n` });
    expect(doc.getSnapshot().diagnostics.filter(d => d.code === 'expression-reference')).toEqual([]);
    expect(() => doc.renameEntry(['jobs', 'call', 'outputs', 'missing'], 'new')).toThrow();
  });
  it('does not rewrite a nested data property that happens to resemble an expression root', () => {
    const doc = new WorkflowDocument({ source });
    doc.setField(['on', 'workflow_call', 'inputs'], { task: { type: 'string' }, config: { type: 'object' } });
    doc.setField(['jobs', 'build', 'steps', 0, 'with', 'prompt'], '${{ inputs.config.inputs.task }} ${{ inputs.task }}');
    doc.renameEntry(['on', 'workflow_call', 'inputs', 'task'], 'request');
    expect(doc.getSnapshot().workflow!.jobs.build.steps![0].with!.prompt).toBe('${{ inputs.config.inputs.task }} ${{ inputs.request }}');
    expect(references('${{ inputs.config.needs.build.outputs.x }}')).toEqual([expect.objectContaining({ scope: 'inputs', id: 'config' })]);
  });
  it('preserves literal defaults, metadata, shell, timeout and imported fields during reference edits', () => {
    const text = '${{ inputs.task }}';
    const doc = new WorkflowDocument({ source });
    doc.setField(['on', 'workflow_call', 'inputs'], { task: { type: 'string', default: text }, untouched: { type: 'string', default: '${{ inputs.missing }}' } });
    doc.setField(['name'], text);
    doc.setField(['jobs', 'build', 'name'], text);
    doc.setField(['jobs', 'build', 'steps', 1], { name: text, shell: text, run: 'echo okay', timeout: '${{ inputs.missing }}' });
    doc.setField(['imported'], { value: text });
    const entry = ['on', 'workflow_call', 'inputs', 'task'];
    expect(doc.entryDeletionImpact(entry)).toEqual([['jobs', 'build', 'steps', 0, 'with', 'prompt']]);
    expect(doc.getSnapshot().diagnostics.filter(d => d.code === 'expression-reference')).toEqual([]);
    doc.renameEntry(entry, 'request');
    const workflow = doc.getSnapshot().workflow!;
    expect(workflow.on!.workflow_call).toMatchObject({ inputs: { request: { default: text } } });
    expect(workflow.name).toBe(text); expect(workflow.jobs.build.name).toBe(text);
    expect(workflow.jobs.build.steps![1]).toMatchObject({ name: text, shell: text, timeout: '${{ inputs.missing }}' });
    expect(workflow.imported).toEqual({ value: text });
    expect(workflow.jobs.build.steps![0].with!.prompt).toContain('inputs.request');
  });
  it('rewrites recursively rendered mappings, command fields and callable output values', () => {
    const doc = new WorkflowDocument({ source });
    const text = '${{ inputs.task }}';
    doc.setField(['on', 'workflow_call'], { inputs: { task: { type: 'string' } }, outputs: { result: { value: text } } });
    doc.setField(['jobs', 'build', 'steps', 0, 'with', 'output'], { schema: { description: text } });
    doc.setField(['jobs', 'build', 'steps', 1], { shell: 'sh', run: `echo ${text}`, 'working-directory': text });
    doc.setField(['jobs', 'call'], { uses: 'workflows/child.yml', with: { object: { list: [text] } }, secrets: { token: text } });
    doc.renameEntry(['on', 'workflow_call', 'inputs', 'task'], 'request');
    const workflow = doc.getSnapshot().workflow!;
    expect(workflow.on!.workflow_call).toMatchObject({ outputs: { result: { value: '${{ inputs.request }}' } } });
    expect(workflow.jobs.build.steps![0].with!.output).toEqual({ schema: { description: '${{ inputs.request }}' } });
    expect(workflow.jobs.build.steps![1]).toMatchObject({ run: 'echo ${{ inputs.request }}', 'working-directory': '${{ inputs.request }}' });
    expect(workflow.jobs.call.with).toEqual({ object: { list: ['${{ inputs.request }}'] } });
    expect(workflow.jobs.call.secrets).toEqual({ token: '${{ inputs.request }}' });
    doc.deleteEntry(['on', 'workflow_call', 'inputs', 'request'], true);
    expect(doc.getSnapshot().diagnostics.filter(d => d.code === 'expression-reference').map(d => d.path)).toEqual(expect.arrayContaining([
      ['jobs', 'call', 'with', 'object', 'list', 0], ['jobs', 'call', 'secrets', 'token'], ['on', 'workflow_call', 'outputs', 'result', 'value'],
    ]));
  });
  it('requires confirmation and diagnoses dangling expressions', () => {
    const doc = new WorkflowDocument({ source }); expect(doc.deletionImpact('build').length).toBe(2);
    expect(() => doc.deleteJob('build')).toThrow('confirmation'); doc.deleteJob('build', true);
    expect(doc.getSnapshot().workflow!.jobs.test.needs).toEqual([]);
    expect(doc.getSnapshot().diagnostics.some(d => d.code === 'expression-reference')).toBe(true);
  });
  it('rejects duplicate, self and cyclic edges', () => {
    const doc = new WorkflowDocument({ source });
    expect(() => doc.connect('build', 'test')).toThrow('duplicate');
    expect(() => doc.connect('build', 'build')).toThrow('Self');
    expect(() => doc.connect('test', 'build')).toThrow('cycle');
    expect(doc.getSnapshot().source).toBe(source);
  });
  it('groups consecutive YAML typing while keeping visual edits separate', () => {
    const doc = new WorkflowDocument({ source });
    doc.setSource('', 'yaml'); doc.setSource('jobs: [broken', 'yaml');
    doc.undo(); expect(doc.getSnapshot().source).toBe(source);
    doc.setField(['name'], 'Visual');
    const visual = doc.getSnapshot().source;
    doc.setSource('jobs: {}', 'yaml'); doc.undo(); expect(doc.getSnapshot().source).toBe(visual);
    doc.undo(); expect(doc.getSnapshot().source).toBe(source);
  });
  it('retains stale graph, suspends mutations and recovers across shared undo', () => {
    const doc = new WorkflowDocument({ source }); doc.setField(['name'], 'Visual');
    const visual = doc.getSnapshot().source; doc.setSource('jobs: [broken');
    expect(doc.getSnapshot()).toMatchObject({ stale: true, source: 'jobs: [broken', workflow: undefined, previewWorkflow: { name: 'Visual' } });
    expect(() => doc.addJob()).toThrow('suspended'); doc.undo(); expect(doc.getSnapshot().source).toBe(visual);
    doc.undo(); expect(doc.getSnapshot().source).toBe(source); doc.redo(); expect(doc.getSnapshot().source).toBe(visual);
    doc.setSource(source); expect(doc.getSnapshot().stale).toBe(false);
  });
  it('duplicates nodes without losing comments and gives new steps unique IDs', () => {
    const doc = new WorkflowDocument({ source }); doc.duplicateStep('build', 0); doc.addStep('build');
    const ids = doc.getSnapshot().workflow!.jobs.build.steps!.map(s => s.id);
    expect(new Set(ids).size).toBe(ids.length); expect(doc.getSnapshot().source).toContain('# prompt comment');
    expect(doc.duplicateJob('build')).toBe('build_copy');
  });
  it('keeps layout out of source and allows read-only navigation', () => {
    const doc = new WorkflowDocument({ source }, true); const changed = vi.fn(); doc.update({ onLayoutChange: changed });
    doc.setLayout({ positions: { build: { x: 10, y: 20 } }, collapsed: {} }); expect(changed).toHaveBeenCalledOnce();
    doc.setSource('no'); doc.undo(); expect(() => doc.addJob()).toThrow('read-only'); expect(doc.getSnapshot().source).toBe(source);
  });
  it('reports save failures and accepts drafts', async () => {
    const doc = new WorkflowDocument({ source: 'bad: [', onSave: async s => { expect(s.stale).toBe(true); throw new Error('disk full'); } });
    await doc.save(); expect(doc.getSnapshot().saveError).toContain('disk full'); expect(doc.getSnapshot().saving).toBe(false);
  });
  it('ignores stale asynchronous validation and exposes failures', async () => {
    const responses: ((value: []) => void)[] = [];
    const doc = new WorkflowDocument({ source, validate: () => new Promise(resolve => responses.push(resolve)) });
    doc.start(); doc.setSource('jobs: {}'); responses[0]([]); await Promise.resolve();
    expect(doc.getSnapshot().validating).toBe(true); responses[1]([]); await Promise.resolve(); expect(doc.getSnapshot().validating).toBe(false);
    doc.update({ validate: async () => { throw new Error('offline'); } }); await Promise.resolve();
    expect(doc.getSnapshot().diagnostics.some(d => d.code === 'host-validation')).toBe(true);
  });
});

describe('host option updates', () => {
  it('does not restart validation for presentation or equivalent catalogs, and allows a context refresh', async () => {
    const validate = vi.fn(() => []);
    const document = new WorkflowDocument({ source, validate, agents: [{ id: 'claude' }] });
    await document.validate(); expect(validate).toHaveBeenCalledTimes(1);
    document.update({ theme: 'dark' });
    document.update({ agents: [{ id: 'claude' }] });
    expect(validate).toHaveBeenCalledTimes(1);
    document.update({ validationKey: 'updated-policy' });
    expect(validate).toHaveBeenCalledTimes(2);
  });
  it('clears catalog warnings when the catalog is removed', async () => {
    const document = new WorkflowDocument({ source, agents: [{ id: 'other' }] });
    await document.validate(); expect(document.getSnapshot().diagnostics.some(d => d.code === 'agent-catalog')).toBe(true);
    document.update({ agents: undefined });
    expect(document.getSnapshot().diagnostics.some(d => d.code === 'agent-catalog')).toBe(false);
  });
});

describe('editing optional null mappings', () => {
  it('materializes null repeat ancestors only on an explicit edit and keeps comments', () => {
    const source = 'jobs:\n  build:\n    repeat: null # keep repeat comment\n    steps: [{argv: [echo]}]\n';
    const document = new WorkflowDocument({ source });
    document.setField(['name'], 'Other edit');
    expect(document.getSnapshot().workflow!.jobs.build.repeat).toBeNull();
    document.setField(['jobs', 'build', 'repeat', 'max-attempts'], 3);
    document.setField(['jobs', 'build', 'repeat', 'until'], 'true');
    expect(document.getSnapshot().workflow!.jobs.build.repeat).toEqual({ 'max-attempts': 3, until: 'true' });
    expect(document.getSnapshot().source).toContain('# keep repeat comment');
    expect(document.getSnapshot().diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    document.undo(); document.undo();
    expect(document.getSnapshot().workflow!.jobs.build.repeat).toBeNull();
  });
});

describe('source cursor context', () => {
  it('locates job keys and step definitions in the original source without editing it', () => {
    const document = new WorkflowDocument({ source });
    const job = document.getSourceRange(['jobs', 'build'])!;
    expect(source.slice(job.from, job.to)).toMatch(/^build:/);
    const step = document.getSourceRange(['jobs', 'build', 'steps', 1])!;
    expect(source.slice(step.from, step.to)).toContain('argv: [echo, done]');
    expect(document.getSnapshot().source).toBe(source);
    expect(document.getSourceRange(['jobs', 'missing'])).toBeUndefined();
    document.setSource('jobs: [');
    expect(document.getSourceRange(['jobs', 'build'])).toBeUndefined();
  });
  it('finds the job and step under the YAML cursor, independently of graph selection', () => {
    const document = new WorkflowDocument({ source });
    expect(document.getPathAtOffset(source.indexOf('Hello') + 2)).toEqual(['jobs', 'build', 'steps', 0, 'with', 'prompt']);
    expect(document.getPathAtOffset(source.indexOf('needs.build.outputs') + 4)).toEqual(['jobs', 'test', 'steps', 0, 'argv', 1]);
    expect(document.getPathAtOffset(source.indexOf('Example') + 2)).toEqual(['name']);
  });
});


describe('asynchronous host lifecycle', () => {
  it('aborts superseded validation and ignores late results after destroy', async () => {
    const requests: { signal: AbortSignal; resolve: (value: import('../src/types').Diagnostic[]) => void }[] = [];
    const changed = vi.fn();
    const document = new WorkflowDocument({ source, onChange: changed, validate: (_, signal) => new Promise(resolve => requests.push({ signal, resolve })) });
    document.start();
    document.setField(['name'], 'Updated');
    expect(requests[0].signal.aborted).toBe(true);
    expect(requests[1].signal.aborted).toBe(false);
    requests[0].resolve([{ code: 'obsolete', message: 'Old result', severity: 'error', source: 'host', path: [] }]);
    await Promise.resolve();
    expect(document.getSnapshot().validating).toBe(true);
    expect(document.getSnapshot().diagnostics.some(item => item.code === 'obsolete')).toBe(false);
    document.destroy();
    expect(requests[1].signal.aborted).toBe(true);
    const calls = changed.mock.calls.length;
    const snapshot = document.getSnapshot();
    requests[1].resolve([{ code: 'disposed', message: 'Disposed result', severity: 'error', source: 'host', path: [] }]);
    await Promise.resolve();
    expect(document.getSnapshot()).toBe(snapshot);
    expect(changed).toHaveBeenCalledTimes(calls);
  });
  it('compares exact saved content across edits, undo and host updates', async () => {
    const document = new WorkflowDocument({ source, onSave: () => {} });
    expect(document.getSnapshot().dirty).toBe(true);
    await document.save();
    expect(document.getSnapshot().dirty).toBe(false);
    document.setField(['name'], 'Edited');
    expect(document.getSnapshot().dirty).toBe(true);
    document.undo();
    expect(document.getSnapshot().dirty).toBe(false);
    document.redo();
    expect(document.getSnapshot().dirty).toBe(true);
    document.update({ source: 'name: Incoming\njobs: {}' });
    document.resolveSourceReplacement('use-incoming');
    expect(document.getSnapshot().dirty).toBe(true);
    document.undo(); document.undo();
    expect(document.getSnapshot().dirty).toBe(false);
    document.update({ source: 'jobs: [' });
    document.resolveSourceReplacement('use-incoming');
    expect(document.getSnapshot().dirty).toBe(true);
    await document.save();
    expect(document.getSnapshot()).toMatchObject({ dirty: false, stale: true });
  });
  it('retains the submitted source when edits or inspector drafts arrive during saving', async () => {
    let finish = () => {};
    const onSave = vi.fn((_snapshot: { source: string }) => new Promise<void>(resolve => { finish = resolve; }));
    const document = new WorkflowDocument({ source, onSave });
    const pending = document.save();
    document.setField(['name'], 'New revision');
    await document.save();
    expect(onSave).toHaveBeenCalledTimes(1);
    finish(); await pending;
    expect(document.getSnapshot()).toMatchObject({ dirty: true, saving: false, hasLocalChanges: true });
    expect(onSave.mock.calls[0][0].source).toBe(source);
    document.undo();
    expect(document.getSnapshot()).toMatchObject({ dirty: false, hasLocalChanges: false });
    document.setInspectorDirty(true);
    const next = document.save(); finish(); await next;
    expect(document.getSnapshot().dirty).toBe(true);
    document.setInspectorDirty(false);
    expect(document.getSnapshot().dirty).toBe(false);
  });
  it('pins submitted content before notifying the host and protects it from callback mutations', async () => {
    let changed = false;
    const onSave = vi.fn((snapshot: any) => { snapshot.source = 'callback mutation'; });
    const document = new WorkflowDocument({ source, onSave, onChange: snapshot => {
      if (snapshot.saving && !changed) { changed = true; document.setSource('jobs: ['); }
    } });
    await document.save();
    expect(document.getSnapshot()).toMatchObject({ source: 'jobs: [', dirty: true });
    document.undo();
    expect(document.getSnapshot()).toMatchObject({ source, dirty: false });
  });
  it('ignores save failure for an older revision and allows saving the current draft', async () => {
    let reject: (error: Error) => void = () => {};
    const save = vi.fn(() => new Promise<void>((_, fail) => { reject = fail; }));
    const document = new WorkflowDocument({ source, onSave: save });
    const pending = document.save();
    document.setField(['name'], 'New revision');
    reject(new Error('Old draft failed'));
    await pending;
    expect(document.getSnapshot()).toMatchObject({ saving: false, saveError: undefined });
    document.update({ onSave: async snapshot => { expect(snapshot.workflow?.name).toBe('New revision'); } });
    await document.save();
    expect(document.getSnapshot()).toMatchObject({ saving: false, saveError: undefined });
  });
  it('does not notify a destroyed instance when a pending save fails', async () => {
    let reject: (error: Error) => void = () => {};
    const changed = vi.fn();
    const document = new WorkflowDocument({ source, onChange: changed, onSave: () => new Promise<void>((_, fail) => { reject = fail; }) });
    const pending = document.save();
    document.destroy();
    const snapshot = document.getSnapshot();
    const calls = changed.mock.calls.length;
    reject(new Error('After teardown'));
    await pending;
    expect(document.getSnapshot()).toBe(snapshot);
    expect(changed).toHaveBeenCalledTimes(calls);
  });
});


describe('host source replacement protection', () => {
  it('starts clean independently from saving and protects edits through echoes', () => {
    const doc = new WorkflowDocument({ source });
    expect(doc.getSnapshot()).toMatchObject({ hasLocalChanges: false, dirty: true });
    doc.setField(['name'], 'Local');
    const local = doc.getSnapshot().source;
    doc.update({ source }); // unchanged initial prop
    doc.update({ source: local });
    expect(doc.getSnapshot()).toMatchObject({ source: local, hasLocalChanges: true, pendingSourceReplacement: undefined });
    // A host that changes back to the original after echoing a local edit
    // is requesting a real replacement, rather than repeating an initial prop.
    doc.update({ source });
    expect(doc.getSnapshot().pendingSourceReplacement).toBe(source);
    doc.resolveSourceReplacement('keep-local');
    const incoming = 'name: Incoming\njobs: {}';
    doc.update({ source: incoming });
    expect(doc.getSnapshot()).toMatchObject({ source: local, pendingSourceReplacement: incoming });
    doc.resolveSourceReplacement('keep-local');
    doc.update({ source: local }); doc.update({ source: incoming });
    expect(doc.getSnapshot()).toMatchObject({ source: local, hasLocalChanges: true, pendingSourceReplacement: undefined });
  });
  it('accepts clean replacements and viewers immediately', () => {
    for (const readOnly of [false, true]) {
      const doc = new WorkflowDocument({ source }, readOnly);
      doc.update({ source: 'jobs: {}' });
      expect(doc.getSnapshot()).toMatchObject({ source: 'jobs: {}', hasLocalChanges: false, pendingSourceReplacement: undefined });
      doc.undo();
      expect(doc.getSnapshot().source).toBe(readOnly ? 'jobs: {}' : source);
    }
  });
  it('protects inspector text and uses the newest candidate as one undoable edit', () => {
    const doc = new WorkflowDocument({ source });
    const discard = vi.fn(); doc.onDiscardInspector(discard);
    doc.setInspectorDirty(true);
    doc.update({ source: 'jobs: {}' });
    doc.update({ source: '' });
    expect(doc.getSnapshot()).toMatchObject({ hasLocalChanges: true, pendingSourceReplacement: '' });
    doc.resolveSourceReplacement('use-incoming');
    expect(discard).toHaveBeenCalledTimes(1);
    expect(doc.getSnapshot()).toMatchObject({ source: '', hasLocalChanges: false, pendingSourceReplacement: undefined });
    doc.undo(); expect(doc.getSnapshot()).toMatchObject({ source, hasLocalChanges: true });
    doc.redo(); expect(doc.getSnapshot().hasLocalChanges).toBe(false);
  });
  it('does not let old saves override accepted replacements or newer inspector protection', async () => {
    let finish = () => {};
    const doc = new WorkflowDocument({ source, onSave: () => new Promise<void>(resolve => { finish = resolve; }) });
    doc.setField(['name'], 'Submitted');
    const pending = doc.save();
    doc.update({ source: 'jobs: {}' }); doc.resolveSourceReplacement('use-incoming');
    doc.setSource(source);
    finish(); await pending;
    expect(doc.getSnapshot().hasLocalChanges).toBe(true);
    doc.update({ source: 'name: Newer\njobs: {}' });
    expect(doc.getSnapshot().pendingSourceReplacement).toContain('Newer');
    doc.resolveSourceReplacement('use-incoming');
    const next = doc.save(); doc.setInspectorDirty(true); finish(); await next;
    expect(doc.getSnapshot().hasLocalChanges).toBe(true);
  });
  it('saves current source as the protection baseline and ignores obsolete validation', async () => {
    const responses: ((value: []) => void)[] = [];
    const doc = new WorkflowDocument({ source, onSave: () => {}, validate: () => new Promise<[]>(resolve => responses.push(resolve)) });
    doc.start(); doc.setField(['name'], 'Saved'); await doc.save();
    expect(doc.getSnapshot().hasLocalChanges).toBe(false);
    doc.update({ source: 'jobs: {}' });
    responses[0]([]); await Promise.resolve();
    expect(doc.getSnapshot()).toMatchObject({ source: 'jobs: {}', hasLocalChanges: false, validating: true });
    responses.at(-1)!([]); await Promise.resolve();
    expect(doc.getSnapshot().validating).toBe(false);
  });
});
