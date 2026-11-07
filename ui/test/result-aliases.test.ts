import { describe, expect, it, vi } from 'vitest';
import { WorkflowDocument } from '../src/document';
import { mapReferences, references, referenceText } from '../src/expressions';
import { expressionSuggestions, stepResultPaths } from '../src/source';
import { parseWorkflow } from '../src/validation';

const workflow = {
  on: { workflow_call: { outputs: { answer: { value: '${{ jobs.review.steps.analyze.approved }}' } } } },
  jobs: {
    review: { steps: [
      { id: 'analyze', agent: 'claude', prompt: 'Review', with: { output: { format: 'json', schema: { type: 'object', properties: { approved: { type: 'boolean' }, details: { type: 'object', properties: { reason: { type: 'string' } } }, status: { type: 'string' } } } } } },
      { id: 'finish', agent: 'claude', prompt: '${{ steps.analyze.approved }}' },
    ] },
    summarize: { needs: ['review'], steps: [{ agent: 'claude', prompt: '${{ needs.review.steps.analyze.details.reason }}' }] },
  },
};

describe('named step result references', () => {
  it('retains every suffix while renaming producers and steps in local, dependency and callable scopes', () => {
    const doc = new WorkflowDocument({ source: JSON.stringify(workflow) });
    doc.renameStep('review', 0, 'inspect'); doc.renameJob('review', 'check');
    const snapshot = doc.getSnapshot();
    expect(snapshot.workflow!.jobs.check.steps![1].prompt).toBe('${{ steps.inspect.approved }}');
    expect(snapshot.workflow!.jobs.summarize.steps![0].prompt).toBe('${{ needs.check.steps.inspect.details.reason }}');
    expect(snapshot.workflow!.on).toMatchObject({ workflow_call: { outputs: { answer: { value: '${{ jobs.check.steps.inspect.approved }}' } } } });
    expect(snapshot.diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    doc.undo(); doc.undo(); expect(doc.getSnapshot().source).toBe(JSON.stringify(workflow));
  });
  it('does not confuse a cross-job step output with an explicit job output', () => {
    const doc = new WorkflowDocument({ source: JSON.stringify(workflow) });
    doc.setField(['jobs', 'review', 'outputs', 'text'], 'declared');
    doc.setField(['jobs', 'summarize', 'steps', 0, 'prompt'], '${{ needs.review.outputs.text }} ${{ needs.review.steps.analyze.outputs.text }}');
    doc.renameEntry(['jobs', 'review', 'outputs', 'text'], 'report');
    expect(doc.getSnapshot().workflow!.jobs.summarize.steps![0].prompt).toBe('${{ needs.review.outputs.report }} ${{ needs.review.steps.analyze.outputs.text }}');
    doc.renameStep('review', 0, 'inspect');
    expect(doc.getSnapshot().workflow!.jobs.summarize.steps![0].prompt).toBe('${{ needs.review.outputs.report }} ${{ needs.review.steps.inspect.outputs.text }}');
  });
  it('preserves implicit step identity through reorder and includes dependent references in deletion review', () => {
    const doc = new WorkflowDocument({ source: 'jobs: {review: {steps: [{argv: [echo]}, {argv: [echo]}]}, summarize: {needs: review, steps: [{agent: claude, prompt: "${{ needs.review.steps.step_1.text }}"}]}}' });
    doc.moveStep('review', 0, 1);
    expect(doc.getSnapshot().workflow!.jobs.review.steps!.map(step => step.id)).toEqual(['step_2', 'step_1']);
    expect(doc.deletionImpact('review', 1)).toContainEqual(['jobs', 'summarize', 'steps', 0, 'prompt']);
    expect(() => doc.deleteStep('review', 1)).toThrow('confirmation');
    doc.deleteStep('review', 1, true);
    expect(doc.getSnapshot().diagnostics).toContainEqual(expect.objectContaining({ code: 'expression-reference', message: 'Unknown step "step_1" of job "review"' }));
  });
  it.each([
    ['forward', 'steps.finish.text', 'review', 0],
    ['forward Unicode field', 'steps.finish.é', 'review', 0],
    ['forward symbol field', 'steps.finish.$value', 'review', 0],
    ['forward numeric field', 'steps.finish.0', 'review', 0],
    ['forward spaced field', 'steps.finish.with space', 'review', 0],
    ['forward metadata prefix', 'steps.finish.outputsé', 'review', 0],
    ['forward error prefix', 'steps.finish.error code', 'review', 0],
    ['self arbitrary field', 'steps.analyze.$value', 'review', 0],
    ['comparison operand', '"literal" == steps.finish.$value', 'review', 0],
    ['escaped comparison operand', String.raw`'\' == steps.finish.text'`, 'review', 0],
    ['logical operand', 'true and not steps.finish.é', 'review', 0],
    ['undeclared dependency', 'needs.review.steps.analyze.text', 'isolated', 0],
    ['job root inside a job', 'jobs.review.steps.analyze.text', 'summarize', 0],
    ['unknown dependency step', 'needs.review.steps.absent.text', 'summarize', 0],
  ])('diagnoses %s at its consuming field', (_label, expression, job, index) => {
    const value = structuredClone(workflow);
    Object.assign(value.jobs, { isolated: { steps: [{ agent: 'claude', prompt: 'Read' }] } });
    const doc = new WorkflowDocument({ source: JSON.stringify(value) });
    const path = ['jobs', job, 'steps', index, 'prompt'];
    doc.setField(path, '${{ ' + expression + ' }}');
    expect(doc.getSnapshot().diagnostics).toContainEqual(expect.objectContaining({ code: 'expression-reference', path, severity: 'error' }));
  });
  it('rejects reusable and per-item producer internals while retaining legacy references', () => {
    const doc = new WorkflowDocument({ source: JSON.stringify(workflow) });
    doc.setField(['jobs', 'review'], { uses: 'workflows/review.yml' });
    expect(doc.getSnapshot().diagnostics.some(d => d.message.includes('does not expose individual step results'))).toBe(true);
    doc.setField(['jobs', 'review', 'for-each'], { items: ['one'], key: 'item' });
    expect(doc.getSnapshot().diagnostics.some(d => d.message.includes('does not expose individual step results'))).toBe(true);
    expect(parseWorkflow('jobs: {review: {steps: [{id: one, argv: [echo, "${{ steps.one.status }}"]}]}}').diagnostics.filter(d => d.code === 'expression-reference')).toEqual([]);
  });
  it('offers known structured fields and direct dependency text without in-flight or reserved aliases', () => {
    const parsed = parseWorkflow(JSON.stringify(workflow));
    const local = expressionSuggestions(parsed.workflow, 'review', 1);
    expect(local).toEqual(expect.arrayContaining(['steps.analyze.text', 'steps.analyze.approved', 'steps.analyze.details.reason', 'steps.analyze.outputs.structured']));
    expect(local).not.toContain('steps.analyze.status'); expect(local).not.toContain('steps.finish.text');
    expect(expressionSuggestions(parsed.workflow, 'summarize', 0)).toContain('needs.review.steps.analyze.approved');
    expect(expressionSuggestions(parsed.workflow)).toContain('jobs.review.steps.analyze.text');
    expect(expressionSuggestions(parsed.workflow, 'review')).not.toContain('steps.analyze.text');
    expect(expressionSuggestions(parsed.workflow, 'review', undefined, ['jobs', 'review', 'outputs', 'result'])).toContain('steps.analyze.text');
  });
  it('only offers concise results for agent producers in every scope', () => {
    const parsed = parseWorkflow(JSON.stringify({ jobs: {
      build: { steps: [
        { id: 'command', argv: ['echo', 'ready'] },
        { id: 'shell', run: 'echo ready', shell: 'sh' },
        { id: 'controller', uses: 'workspace/create' },
        { id: 'legacy', uses: 'agent/claude', with: { prompt: 'Review', output: { format: 'json', schema: { type: 'object', properties: { approved: { type: 'boolean' } } } } } },
        { id: 'concise', agent: 'claude', prompt: 'Summarize' },
        { id: 'consume', agent: 'claude', prompt: 'Read' },
      ] },
      consumer: { needs: ['build'], steps: [{ agent: 'claude', prompt: 'Read' }] },
    } }));
    expect(parsed.diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    for (const [suggestions, prefix] of [
      [expressionSuggestions(parsed.workflow, 'build', 5), 'steps'],
      [expressionSuggestions(parsed.workflow, 'consumer', 0), 'needs.build.steps'],
      [expressionSuggestions(parsed.workflow), 'jobs.build.steps'],
    ] as const) {
      expect(suggestions).toContain(`${prefix}.legacy.text`);
      expect(suggestions).toContain(`${prefix}.legacy.approved`);
      expect(suggestions).toContain(`${prefix}.concise.text`);
      for (const id of ['command', 'shell', 'controller']) expect(suggestions).not.toContain(`${prefix}.${id}.text`);
    }
    expect(expressionSuggestions(parsed.workflow, 'build', 5)).toContain('steps.command.outputs.stdout');
  });
  it('keeps data roots and quoted expressions distinct from cross-job step references', () => {
    expect(references("${{ 'needs.review.steps.analyze.text' == needs.review.steps.analyze.outputs.structured.status }}")).toEqual([
      expect.objectContaining({ scope: 'needs', id: 'review', step: 'analyze', output: 'structured', tail: '.status' }),
    ]);
    expect(references('${{ inputs.config.needs.review.steps.analyze.text }}')).toEqual([expect.objectContaining({ scope: 'inputs', id: 'config', step: undefined })]);
  });
  it.each(['.', '..', '.outputs.text', '..outputs.text', '...status', '..text'])('preserves legacy components in steps.analyze%s during diagnostics and rename', suffix => {
    const doc = new WorkflowDocument({ source: JSON.stringify(workflow) });
    const condition = `false and steps.analyze${suffix}`;
    doc.setField(['jobs', 'review', 'if'], condition);
    const source = doc.getSnapshot().source;
    expect(doc.getSnapshot().diagnostics.filter(d => d.code === 'expression-reference')).toEqual([]);
    doc.renameStep('review', 0, 'inspect');
    expect(doc.getSnapshot().workflow!.jobs.review.if).toBe(`false and steps.inspect${suffix}`);
    doc.undo(); expect(doc.getSnapshot().source).toBe(source);
  });
  it('preserves arbitrary suffixes, operand offsets, whitespace and quoted literals during renames', () => {
    const body = "  'steps.analyze.$value and needs.review.steps.analyze.é' == steps.analyze.$value and not needs.review.steps.analyze.with space  ";
    expect(references('${{' + body + '}}')).toEqual([
      { scope: 'steps', id: 'analyze', step: undefined, output: undefined, tail: '.$value', offset: body.indexOf('steps.analyze.$value and not') },
      { scope: 'needs', id: 'review', step: 'analyze', output: undefined, tail: '.with space', offset: body.lastIndexOf('needs.review') },
    ]);
    expect(mapReferences('${{' + body + '}}', ref => referenceText({ ...ref, ...(ref.scope === 'steps' ? { id: 'inspect' } : { step: 'inspect' }) }))).toBe(
      "${{  'steps.analyze.$value and needs.review.steps.analyze.é' == steps.inspect.$value and not needs.review.steps.inspect.with space  }}");
    const doc = new WorkflowDocument({ source: JSON.stringify(workflow) });
    doc.setField(['jobs', 'review', 'steps', 1, 'prompt'], '${{ steps.analyze.outputsé }} ${{ steps.analyze.0.é }}');
    doc.setField(['jobs', 'summarize', 'steps', 0, 'prompt'], '${{ needs.review.steps.analyze.with space }}');
    doc.renameStep('review', 0, 'inspect');
    expect(doc.getSnapshot().workflow!.jobs.review.steps![1].prompt).toBe('${{ steps.inspect.outputsé }} ${{ steps.inspect.0.é }}');
    expect(doc.getSnapshot().workflow!.jobs.summarize.steps![0].prompt).toBe('${{ needs.review.steps.inspect.with space }}');
    expect(doc.getSnapshot().diagnostics.filter(d => d.code === 'expression-reference')).toEqual([]);
  });
  it.each(['\u0085', '\u00a0', '\u2007', '\u202f'])('tracks runner whitespace %j through rename, deletion review and Undo', space => {
    const prompt = '${{' + space + 'steps.analyze.outputs.text' + space + '}}';
    const condition = space + '${{' + space + 'not ' + space + 'steps.analyze.text' + space + '}}' + space;
    const doc = new WorkflowDocument({ source: JSON.stringify(workflow) });
    doc.setField(['jobs', 'review', 'steps', 1, 'prompt'], prompt);
    doc.setField(['jobs', 'review', 'steps', 1, 'if'], condition);
    const source = doc.getSnapshot().source;
    expect(references(prompt)).toEqual([expect.objectContaining({ id: 'analyze', output: 'text', offset: space.length })]);
    expect(references(condition, true)).toEqual([expect.objectContaining({ id: 'analyze', tail: '.text', offset: space.length + 4 + space.length })]);
    expect(doc.deletionImpact('review', 0)).toEqual(expect.arrayContaining([
      ['jobs', 'review', 'steps', 1, 'prompt'], ['jobs', 'review', 'steps', 1, 'if'],
    ]));
    doc.renameStep('review', 0, 'inspect');
    expect(doc.getSnapshot().workflow!.jobs.review.steps![1].prompt).toBe(prompt.replace('analyze', 'inspect'));
    expect(doc.getSnapshot().workflow!.jobs.review.steps![1].if).toBe(condition.replace('analyze', 'inspect'));
    doc.undo(); expect(doc.getSnapshot().source).toBe(source);
    doc.setField(['jobs', 'review', 'steps', 0, 'if'], condition);
    expect(doc.getSnapshot().diagnostics).toContainEqual(expect.objectContaining({ code: 'expression-reference', path: ['jobs', 'review', 'steps', 0, 'if'] }));
  });
  it('keeps BOM characters in operands because the runner does not trim them', () => {
    expect(references('${{ \ufeffsteps.analyze.text }}')).toEqual([]);
    expect(references('${{ steps.analyze.text\ufeff }}')).toEqual([expect.objectContaining({ tail: '.text\ufeff' })]);
    expect(mapReferences('${{ \u0085steps.analyze.text\ufeff\u0085 }}', ref => referenceText({ ...ref, id: 'inspect' }))).toBe('${{ \u0085steps.inspect.text\ufeff\u0085 }}');
  });
  it('checks job-level arbitrary-field references while preserving legacy fields and literals', () => {
    const doc = new WorkflowDocument({ source: JSON.stringify(workflow) });
    doc.setField(['jobs', 'review', 'if'], 'steps.analyze.with space');
    expect(doc.getSnapshot().diagnostics).toContainEqual(expect.objectContaining({ code: 'expression-reference', path: ['jobs', 'review', 'if'] }));
    doc.setField(['jobs', 'review', 'if'], 'steps.analyze.status');
    doc.setField(['jobs', 'review', 'steps', 0, 'prompt'], '${{ "steps.finish.$value" }} plain steps.finish.é');
    expect(doc.getSnapshot().diagnostics.filter(d => d.code === 'expression-reference')).toEqual([]);
  });
  it.each([
    ['${{ "}}" == "}}" and steps.analyze.text }}', '.text'],
    [String.raw`'\' == steps.analyze.text'`, ".text'"],
  ])('tracks executable condition operands in %s through rename and Undo', (condition, tail) => {
    const doc = new WorkflowDocument({ source: JSON.stringify(workflow) });
    const path = ['jobs', 'review', 'steps', 1, 'if'];
    doc.setField(path, condition);
    const source = doc.getSnapshot().source;
    expect(references(condition, true)).toEqual([
      expect.objectContaining({ scope: 'steps', id: 'analyze', tail }),
    ]);
    doc.renameStep('review', 0, 'inspect');
    expect(doc.getSnapshot().workflow!.jobs.review.steps![1].if).toBe(condition.replace('steps.analyze.', 'steps.inspect.'));
    doc.undo(); expect(doc.getSnapshot().source).toBe(source);
    doc.setField(path, condition.replace('steps.analyze.', 'steps.finish.'));
    expect(doc.getSnapshot().diagnostics).toContainEqual(expect.objectContaining({ code: 'expression-reference', path }));
  });
  it.each(['true and ', 'not ', 'true == '])('keeps long %s conditions editable with reference offsets intact', prefix => {
    const condition = prefix.repeat(10_000) + 'inputs.task';
    const source = JSON.stringify({ on: { workflow_call: { inputs: { task: { type: 'boolean' } } } }, jobs: {
      build: { if: condition, steps: [{ argv: ['echo', 'ready'] }] },
    } });
    const doc = new WorkflowDocument({ source });
    expect(doc.getSnapshot().stale).toBe(false);
    expect(doc.getSnapshot().diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    expect(references(condition, true)).toEqual([
      { scope: 'inputs', id: 'task', step: undefined, output: undefined, tail: undefined, offset: prefix.length * 10_000 },
    ]);
    doc.renameEntry(['on', 'workflow_call', 'inputs', 'task'], 'ready');
    expect(doc.getSnapshot().workflow!.jobs.build.if).toBe(prefix.repeat(10_000) + 'inputs.ready');
    doc.undo(); expect(doc.getSnapshot().source).toBe(source);
  });
  it.each([' == ', ' != ', ' >= ', ' <= ', ' > ', ' < '])('keeps quoted %s chains editable without repeated character scans', operator => {
    const prefix = 'true' + operator;
    const condition = prefix.repeat(50_000) + "'literal and text'" + operator + 'inputs.task';
    const source = JSON.stringify({ on: { workflow_call: { inputs: { task: { type: 'boolean' } } } }, jobs: {
      build: { if: condition, steps: [{ argv: ['echo', 'ready'] }] },
    } });
    const startsWith = String.prototype.startsWith;
    let scans = 0;
    // Stop a regressed synchronous character scan locally before it blocks the worker.
    const scan = vi.spyOn(String.prototype, 'startsWith').mockImplementation(function (this: string, search, position) {
      if (++scans > 2_000_000) throw new Error('Expression character scan budget exceeded');
      return startsWith.call(this, search, position);
    });
    try {
      const doc = new WorkflowDocument({ source });
      expect(doc.getSnapshot().stale).toBe(false);
      expect(doc.getSnapshot().diagnostics.filter(d => d.severity === 'error')).toEqual([]);
      expect(references(condition, true)).toEqual([
        { scope: 'inputs', id: 'task', step: undefined, output: undefined, tail: undefined, offset: condition.indexOf('inputs.task') },
      ]);
      doc.renameEntry(['on', 'workflow_call', 'inputs', 'task'], 'ready');
      expect(doc.getSnapshot().workflow!.jobs.build.if).toBe(prefix.repeat(50_000) + "'literal and text'" + operator + 'inputs.ready');
      doc.undo(); expect(doc.getSnapshot().source).toBe(source);
    } finally { scan.mockRestore(); }
  });
  it('bounds producer step searches for reference-heavy conditions', () => {
    const source = JSON.stringify({ jobs: {
      producer: { steps: Array.from({ length: 1_000 }, () => ({ argv: ['echo', 'lookup-budget'] })) },
      consumer: { needs: ['producer'], if: 'false and ' + 'needs.producer.steps.step_1000.status and '.repeat(10_000) + 'true', steps: [{ argv: ['echo', 'skipped'] }] },
    } });
    const some = Array.prototype.some;
    let visits = 0;
    const search = vi.spyOn(Array.prototype, 'some').mockImplementation(function (this: unknown[], callback, thisArg) {
      return some.call(this, (value, index, array) => {
        if (value?.argv?.[1] === 'lookup-budget' && ++visits > 20_000) throw new Error('Producer step search budget exceeded');
        return callback.call(thisArg, value, index, array);
      });
    });
    try {
      const parsed = parseWorkflow(source);
      expect(parsed.usable).toBe(true);
      expect(parsed.diagnostics.filter(d => d.severity === 'error')).toEqual([]);
      expect(visits).toBeLessThan(20_000);
    } finally { search.mockRestore(); }
  });
  it.each([
    ["'literal == inputs.task and inputs.other", ['task', 'other']],
    ['"literal != inputs.task and inputs.other', ['task', 'other']],
    ["'escaped\\' and literal' == inputs.task", ['task']],
    ['"escaped\\" and literal" == inputs.task', ['task']],
    ["'literal and text' == inputs.task and inputs.other", ['task', 'other']],
  ])('preserves quote boundaries after operand splits in %s', (condition, ids) => {
    expect(references(condition, true).map(ref => ref.id)).toEqual(ids);
    expect(mapReferences(condition, ref => referenceText({ ...ref, id: 'renamed' }), true)).toBe(
      condition.replace(/inputs\.(task|other)/g, 'inputs.renamed'));
  });
  it('resolves named output schemas for local, dependency and workflow completion', () => {
    const value = structuredClone(workflow);
    const schema = value.jobs.review.steps[0].with!.output.schema;
    const named = { ...value, schemas: { Review: schema } };
    Object.assign(named.jobs.review.steps[0].with!.output, { schema: 'Review' });
    const parsed = parseWorkflow(JSON.stringify(named));
    expect(expressionSuggestions(parsed.workflow, 'review', 1)).toContain('steps.analyze.details.reason');
    expect(expressionSuggestions(parsed.workflow, 'summarize', 0)).toContain('needs.review.steps.analyze.approved');
    expect(expressionSuggestions(parsed.workflow)).toContain('jobs.review.steps.analyze.details.reason');
  });
  it('bounds shared-schema suggestions before expansion in root and dependency inspectors while keeping typed paths reachable', () => {
    const producer = (id: string) => ({ id, agent: 'claude', prompt: 'Review', with: { output: { schema: 'Shared' } } });
    const dependencies = Object.fromEntries(Array.from({ length: 100 }, (_, index) => [`producer${index}`, { steps: [producer(`result${index}`)] }]));
    const parsed = parseWorkflow(JSON.stringify({
      schemas: { Shared: { type: 'object', properties: Object.fromEntries(Array.from({ length: 1_000 }, (_, index) => [`field${index}`, { type: 'string' }])) } },
      jobs: {
        producer: { steps: Array.from({ length: 100 }, (_, index) => producer(`result${index}`)) },
        ...dependencies,
        consumer: { needs: ['producer'], steps: [{ agent: 'claude', prompt: 'Read' }] },
        wide: { needs: Object.keys(dependencies), steps: [{ agent: 'claude', prompt: 'Read' }] },
      },
    }));
    expect(parsed.usable).toBe(true);
    expect(parsed.diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    const schema = parsed.workflow!.schemas!.Shared as { properties: Record<string, unknown> };
    let reads = 0;
    schema.properties = new Proxy(schema.properties, { get(target, key) {
      if (++reads > 250) throw new Error('Suggestion schema expansion budget exceeded');
      return Reflect.get(target, key);
    } });
    for (const job of [undefined, 'consumer', 'wide']) {
      reads = 0;
      expect(expressionSuggestions(parsed.workflow, job, job ? 0 : undefined)).toHaveLength(200);
      expect(reads).toBeLessThan(250);
      reads = 0;
      const prefix = `${job ? 'needs' : 'jobs'}.${job === 'wide' ? 'producer99' : 'producer'}.steps.result99.field999`;
      expect(expressionSuggestions(parsed.workflow, job, job ? 0 : undefined, undefined, prefix)).toEqual([prefix]);
      expect(reads).toBeLessThan(250);
    }
  });
  it('bounds a single named schema before materialization and finds fields beyond the initial result-picker list', () => {
    let reads = 0;
    const properties = new Proxy(Object.fromEntries(Array.from({ length: 100_000 }, (_, index) => [`field${index}`, { type: 'string' }])), { get(target, key) {
      if (++reads > 250) throw new Error('Result picker schema expansion budget exceeded');
      return Reflect.get(target, key);
    } });
    const schemas = { Shared: { type: 'object', properties } };
    const producer = { agent: 'claude', with: { output: { schema: 'Shared' } } };
    expect(stepResultPaths(producer, schemas)).toHaveLength(200);
    expect(reads).toBeLessThan(250);
    reads = 0;
    expect(stepResultPaths(producer, schemas, 'field99999')).toEqual(['field99999']);
    expect(reads).toBeLessThan(250);
  });
  it('scans rejected shared-schema keys once per suggestion request across producers and remaining limits', () => {
    const parsed = parseWorkflow(JSON.stringify({
      schemas: { Shared: { type: 'object', properties: Object.fromEntries(Array.from({ length: 1_000 }, (_, index) => [`$field${index}`, {}])) } },
      jobs: {
        producer: { steps: Array.from({ length: 200 }, (_, index) => ({ id: `result${index}`, agent: 'claude', prompt: 'Review', with: { output: { schema: 'Shared' } } })) },
        consumer: { needs: ['producer'], steps: [{ agent: 'claude', prompt: 'Read' }] },
      },
    }));
    expect(parsed.usable).toBe(true);
    expect(parsed.diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    const schema = parsed.workflow!.schemas!.Shared as { properties: Record<string, unknown> };
    let checks = 0;
    schema.properties = new Proxy(schema.properties, { getOwnPropertyDescriptor(target, key) {
      if (++checks > 4_000) throw new Error('Repeated rejected schema-key scan exceeded local budget');
      return Reflect.getOwnPropertyDescriptor(target, key);
    } });
    for (const job of [undefined, 'consumer', 'producer']) {
      checks = 0;
      const suggestions = expressionSuggestions(parsed.workflow, job, job ? 200 : undefined);
      expect(suggestions).toHaveLength(200);
      expect(suggestions).toContain(`${job === 'producer' ? 'steps' : `${job ? 'needs' : 'jobs'}.producer.steps`}.result0.text`);
      expect(checks).toBeLessThanOrEqual(2_000);
    }
    Object.assign(schema.properties, { later: { type: 'string' } });
    checks = 0;
    const prefix = 'needs.producer.steps.result199.later';
    expect(expressionSuggestions(parsed.workflow, 'consumer', 0, undefined, prefix)).toEqual([prefix]);
    expect(checks).toBeLessThanOrEqual(2_002);
  });
  it.each([
    ['preceding', 'steps.first.text', false],
    ['self', 'steps.consume.text', true],
    ['forward', 'steps.last.text', true],
    ['forward arbitrary field', 'steps.last.$value', true],
    ['self arbitrary field', 'steps.consume.outputsé', true],
    ['preceding arbitrary field', 'steps.first.with space', false],
    ['dependency', 'needs.producer.steps.analyze.text', false],
    ['wrong root', 'jobs.producer.steps.analyze.text', true],
  ])('checks %s references inside named schemas in the consuming step context', (_name, expression, invalid) => {
    const parsed = parseWorkflow(JSON.stringify({ schemas: { Result: { type: 'string', enum: ['${{ ' + expression + ' }}'] } }, jobs: {
      producer: { steps: [{ id: 'analyze', agent: 'claude', prompt: 'Produce' }] },
      consumer: { needs: ['producer'], steps: [
        { id: 'first', agent: 'claude', prompt: 'First' },
        { id: 'consume', agent: 'claude', prompt: 'Consume', with: { output: { schema: 'Result' } } },
        { id: 'last', agent: 'claude', prompt: 'Last' },
      ] },
    } }));
    expect(parsed.diagnostics.some(d => d.code === 'expression-reference' && d.path.join('.') === 'schemas.Result.enum.0')).toBe(invalid);
  });
  it('rewrites concise references in shared named contracts and dependent declarations together', () => {
    const doc = new WorkflowDocument({ source: 'schemas: {Shared: {type: string, enum: ["${{ steps.local.text }}"]}, Dependent: {type: string, enum: ["${{ needs.review.steps.local.text }}"]}}\njobs: {review: {steps: [{id: local, agent: claude, prompt: Produce}, {agent: claude, prompt: Consume, with: {output: {schema: Shared}}}]}, second: {steps: [{id: local, agent: claude, prompt: Produce}, {agent: claude, prompt: Consume, with: {output: {schema: Shared}}}]}, summarize: {needs: review, steps: [{agent: claude, prompt: Summarize, with: {output: {schema: Dependent}}}]}}' });
    const original = doc.getSnapshot().source;
    doc.renameStep('review', 0, 'renamed');
    const snapshot = doc.getSnapshot();
    expect(snapshot.workflow!.schemas!.Shared.enum).toEqual(['${{ steps.local.text }}']);
    expect(snapshot.workflow!.schemas!.Shared_review.enum).toEqual(['${{ steps.renamed.text }}']);
    expect(snapshot.workflow!.schemas!.Dependent.enum).toEqual(['${{ needs.review.steps.renamed.text }}']);
    expect(Object.keys(snapshot.workflow!.schemas!)).toEqual(['Shared', 'Dependent', 'Shared_review']);
    expect(snapshot.diagnostics.filter(d => d.code === 'expression-reference')).toEqual([]);
    expect(doc.deletionImpact('review', 0)).toContainEqual(['schemas', 'Dependent', 'enum', 0]);
    doc.undo(); expect(doc.getSnapshot().source).toBe(original);
  });
});

describe('direct named result wiring', () => {
  it('adds only dependency and expression atomically and rejects stale previews', () => {
    const value = structuredClone(workflow); value.jobs.summarize.needs = [];
    const original = JSON.stringify(value);
    const doc = new WorkflowDocument({ source: original });
    const request = { target: ['jobs', 'summarize', 'steps', 0, 'prompt'], producer: 'review', source: { kind: 'result' as const, step: 0, field: 'details.reason' }, selection: { start: 0, end: value.jobs.summarize.steps[0].prompt.length } };
    const preview = doc.previewJobResult(request);
    expect(preview.declaration).toBeUndefined(); expect(doc.getSnapshot().source).toBe(original);
    doc.applyJobResult(preview);
    expect(doc.getSnapshot().workflow!.jobs.summarize.steps![0].prompt).toBe('${{ needs.review.steps.analyze.details.reason }}');
    expect(doc.getSnapshot().workflow!.jobs.review.outputs).toBeUndefined();
    expect(doc.getSnapshot().workflow!.jobs.summarize.needs).toEqual(['review']);
    expect(() => doc.applyJobResult(preview)).toThrow('document changed');
    doc.undo(); expect(doc.getSnapshot().source).toBe(original);
  });
  it('uses workflow scope and validates nested field paths', () => {
    const doc = new WorkflowDocument({ source: JSON.stringify(workflow) });
    const request = { target: ['on', 'workflow_call', 'outputs', 'answer', 'value'], producer: 'review', source: { kind: 'result' as const, step: 0, field: 'outputs.structured.status' } };
    expect(doc.previewJobResult(request).expression).toBe('${{ jobs.review.steps.analyze.outputs.structured.status }}');
    expect(() => doc.previewJobResult({ ...request, source: { ...request.source, field: 'bad..path' } })).toThrow('field path');
    doc.setField(['jobs', 'review'], { uses: 'workflows/review.yml' });
    expect(() => doc.previewJobResult(request)).toThrow('does not expose');
  });
});
