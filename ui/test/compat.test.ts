import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parseWorkflow } from '../src/validation';
import { WorkflowDocument } from '../src/document';
const root = resolve(import.meta.dirname, '../../testdata/ui-compat');
const fixtures = JSON.parse(readFileSync(resolve(root, 'cases.json'), 'utf8')) as { name: string; file: string; parse: boolean; valid: boolean }[];
describe('advisory reference dependencies', () => {
  it.each([
    { name: 'repeated missing dependency', needs: [], prompt: '${{ needs.producer.outputs.text }} ${{ needs.producer.outputs.text }}', condition: '', warnings: 1, errors: 0 },
    { name: 'normalized existing dependency', needs: [' producer '], prompt: '${{ needs.producer.outputs.text }}', condition: '', warnings: 0, errors: 0 },
    { name: 'unknown producer', needs: [], prompt: '${{ needs.absent.outputs.text }}', condition: '', warnings: 0, errors: 1 },
    { name: 'missing output and dependency', needs: [], prompt: '${{ needs.producer.outputs.absent }}', condition: '', warnings: 1, errors: 1 },
    { name: 'bare condition', needs: [], prompt: 'literal', condition: 'needs.producer.outputs.text', warnings: 1, errors: 0 },
    { name: 'literal prompt', needs: [], prompt: 'needs.producer.outputs.text', condition: '', warnings: 0, errors: 0 },
  ])('$name', ({ needs, prompt, condition, warnings, errors }) => {
    const result = parseWorkflow(JSON.stringify({ jobs: {
      producer: { outputs: { text: 'hello' }, steps: [{ argv: ['echo'] }] },
      consumer: { needs, if: condition, steps: [{ uses: 'agent/claude', with: { prompt } }] },
    } }));
    const advisory = result.diagnostics.filter(d => d.code === 'expression-dependency');
    expect(advisory).toHaveLength(warnings);
    expect(advisory.every(d => d.severity === 'warning')).toBe(true);
    expect(result.diagnostics.filter(d => d.code === 'expression-reference')).toHaveLength(errors);
    expect(result.workflow!.jobs.consumer.needs).toEqual(needs.map(id => id.trim()));
  });

  it('bounds repeated advisories while retaining distinct consuming fields and producers', () => {
    const expression = '${{ needs.first.outputs.text }} ${{ needs.second.outputs.text }}';
    const result = parseWorkflow(JSON.stringify({ jobs: {
      first: { outputs: { text: 'hello' }, steps: [{ argv: ['echo'] }] },
      second: { outputs: { text: 'world' }, steps: [{ argv: ['echo'] }] },
      consumer: { if: expression.repeat(200), steps: [{ uses: 'agent/claude', with: { prompt: expression.repeat(200) } }] },
    } }));
    const warnings = result.diagnostics.filter(d => d.code === 'expression-dependency');
    expect(warnings).toHaveLength(4);
    expect(warnings.map(d => d.path)).toEqual([
      ['jobs', 'consumer', 'if'], ['jobs', 'consumer', 'if'],
      ['jobs', 'consumer', 'steps', 0, 'with', 'prompt'], ['jobs', 'consumer', 'steps', 0, 'with', 'prompt'],
    ]);
    for (const producer of ['first', 'second']) expect(warnings.filter(d => d.message.includes(`Job "${producer}"`))).toHaveLength(2);
    expect(result.diagnostics.filter(d => d.severity === 'error')).toEqual([]);
  });

  it('bounds identifier text in dependency messages while retaining full paths', () => {
    const producer = 'producer_' + 'p'.repeat(4096);
    const consumer = 'consumer_' + 'c'.repeat(4096);
    const prompt = '${{ needs.' + producer + '.outputs.text }}';
    const result = parseWorkflow(JSON.stringify({ jobs: {
      [producer]: { outputs: { text: 'hello' }, steps: [{ argv: ['echo'] }] },
      [consumer]: { steps: [{ uses: 'action/capture', with: Object.fromEntries(Array.from({ length: 64 }, (_, i) => ['value_' + i, prompt])) }] },
    } }));
    const warnings = result.diagnostics.filter(d => d.code === 'expression-dependency');
    expect(warnings).toHaveLength(64);
    expect(warnings.every(d => d.message.length < 300 && d.message.includes('…'))).toBe(true);
    expect(warnings.every(d => d.path[1] === consumer)).toBe(true);
    expect(warnings.at(-1)!.path).toEqual(['jobs', consumer, 'steps', 0, 'with', 'value_63']);
    expect(result.diagnostics.filter(d => d.severity === 'error')).toEqual([]);
  });

  it('normalizes dependencies a bounded number of times for repeated references', () => {
    const source = JSON.stringify({ jobs: {
      producer: { outputs: { text: 'hello' }, steps: [{ argv: ['echo'] }] },
      consumer: { needs: Array(100).fill('producer'), steps: [{ uses: 'agent/claude', with: { prompt: '${{ needs.producer.outputs.text }}'.repeat(200) } }] },
    } });
    const trim = String.prototype.trim;
    let normalizations = 0;
    String.prototype.trim = function (this: string) { if (String(this) === 'producer') normalizations++; return trim.call(this); };
    let result;
    try { result = parseWorkflow(source); }
    finally { String.prototype.trim = trim; }
    expect(result.usable).toBe(true);
    expect(result.diagnostics.filter(d => d.code === 'expression-dependency')).toEqual([]);
    // Allow overhead from parsing/structural checks, but exclude work per reference.
    expect(normalizations).toBeLessThan(2000);
  });
});

describe('runner compatibility', () => {
  for (const fixture of fixtures) it(fixture.name, () => {
    const result = parseWorkflow(readFileSync(resolve(root, fixture.file), 'utf8'));
    expect(!result.diagnostics.some(d => d.source === 'parser' && d.severity === 'error')).toBe(fixture.parse);
    expect(!result.diagnostics.some(d => d.severity === 'error')).toBe(fixture.valid);
    if (!result.usable) expect(result.diagnostics.some(d => d.severity === 'error')).toBe(true);
  });
  it('bounds bytes and depth without expanding aliases', () => {
    expect(parseWorkflow('#' + 'a'.repeat(4 * 1024 * 1024)).diagnostics[0]).toMatchObject({ code: 'size', location: { line: 1, column: 1 } });
    expect(parseWorkflow('gates: ' + '['.repeat(66) + '0' + ']'.repeat(66)).diagnostics.some(d => d.code === 'depth')).toBe(true);
  });
  it('attaches a path and source location to unknown fields', () => {
    const result = parseWorkflow('jobs:\n  build:\n    surprise: true\n    steps: [{argv: [echo]}]\n');
    expect(result.diagnostics[0]).toMatchObject({ code: 'unknown-field', path: ['jobs', 'build', 'surprise'], location: { line: 3 } });
  });
});


describe('Go scalar string decoding', () => {
  it('decodes legacy boolean spelling for typed flags without changing raw parameters', () => {
    const result = parseWorkflow('jobs: {test: {continue-on-error: "no", steps: [{uses: agent/claude, continue-on-error: YES, with: {prompt: off}}]}}');
    expect(result.diagnostics).toEqual([]);
    expect(result.workflow!.jobs.test['continue-on-error']).toBe(false);
    expect(result.workflow!.jobs.test.steps![0]['continue-on-error']).toBe(true);
    expect(result.workflow!.jobs.test.steps![0].with!.prompt).toBe('off');
    expect(parseWorkflow(result.doc!.toString()).workflow).toEqual(result.workflow);
  });

  it('normalizes dependency scalars using YAML spelling without editing source', () => {
    const source = 'jobs: {test: {needs: [01, 0xA, 1.0, true, false, null, " build ", ""], steps: [{argv: [echo]}]}}';
    const result = parseWorkflow(source);
    expect(result.workflow!.jobs.test.needs).toEqual(['01', '0xA', '1.0', 'true', 'false', 'build']);
    expect(parseWorkflow(result.doc!.toString()).workflow!.jobs.test.needs).toEqual(result.workflow!.jobs.test.needs);
  });
  it('normalizes trigger and nested entity StringLists consistently', () => {
    const result = parseWorkflow('on: {event: {sources: true, actor: {ids: [01, null], attributes: {state: [false, " active "]}}}}\njobs: {test: {steps: [{argv: [echo]}]}}');
    expect(result.workflow!.on!.event).toEqual({ sources: ['true'], actor: { ids: ['01'], attributes: { state: ['false', 'active'] } } });
  });
  it('keeps runner string semantics through visual serialization', () => {
    const source = 'name: 42\njobs: {TRUE: {outputs: {answer: 42}, steps: [{argv: [echo, 01, true]}]}, test: {needs: TRUE, steps: [{argv: [echo]}]}}';
    const result = parseWorkflow(source);
    expect(result.diagnostics).toEqual([]);
    expect(result.workflow!.name).toBe('42');
    expect(result.workflow!.jobs.TRUE.steps![0].argv).toEqual(['echo', '01', 'true']);
    expect(result.workflow!.jobs.TRUE.outputs).toEqual({ answer: '42' });
    expect(parseWorkflow(result.doc!.toString()).diagnostics).toEqual([]);
    expect(parseWorkflow(result.doc!.toString()).workflow).toEqual(result.workflow);
    const document = new WorkflowDocument({ source });
    expect(document.getSnapshot().source).toBe(source);
    document.setField(['name'], 'Changed');
    expect(document.getSnapshot().diagnostics).toEqual([]);
    expect(document.getSnapshot().workflow!.jobs.test.needs).toEqual(['TRUE']);
    expect(document.getSnapshot().workflow!.jobs.TRUE.steps![0].argv).toEqual(['echo', '01', 'true']);
  });
});


describe('Go repeat integer decoding', () => {
  it.each([['1.5', 1], ['100.9', 100], ['010', 8], ['08', 8], ['1e1', 10]])('decodes %s as %s and preserves semantics through visual edits', (value, expected) => {
    const source = `jobs: {test: {repeat: {max-attempts: ${value}, until: done}, steps: [{argv: [echo]}]}}`;
    const document = new WorkflowDocument({ source });
    expect(document.getSnapshot().source).toBe(source);
    expect(document.getSnapshot().diagnostics).toEqual([]);
    expect(document.getSnapshot().workflow!.jobs.test.repeat!['max-attempts']).toBe(expected);
    document.setField(['name'], 'Changed');
    expect(document.getSnapshot().diagnostics).toEqual([]);
    expect(document.getSnapshot().workflow!.jobs.test.repeat!['max-attempts']).toBe(expected);
  });
  it('preserves explicit scalar tags and runner string meaning through visual edits', () => {
    const source = 'name: !!int 01\njobs: {"TRUE": {repeat: {max-attempts: !!float 1.5, until: done}, steps: [{argv: [echo, !!bool TRUE, !!int 010, !!str 01]}]}, test: {needs: !!bool TRUE, steps: [{argv: [echo]}]}}';
    const document = new WorkflowDocument({ source });
    expect(document.getSnapshot().diagnostics).toEqual([]);
    const before = document.getSnapshot().workflow;
    document.setField(['jobs', 'test', 'name'], 'Changed');
    expect(document.getSnapshot().diagnostics).toEqual([]);
    expect(document.getSnapshot().workflow!.jobs.TRUE).toEqual(before!.jobs.TRUE);
    expect(document.getSnapshot().workflow!.jobs.test.needs).toEqual(['TRUE']);
    expect(document.getSnapshot().workflow!.name).toBe('01');
  });
});


describe('Go null collection decoding', () => {
  it('omits typed slice nulls while retaining raw source and comments', () => {
    const source = 'jobs:\n  test:\n    steps:\n      # preserve skipped step\n      - null # inline skipped comment\n      - argv: [echo, null, done]\n      - null # trailing skipped comment\n';
    const document = new WorkflowDocument({ source });
    expect(document.getSnapshot().source).toBe(source);
    expect(document.getSnapshot().workflow!.jobs.test.steps).toHaveLength(1);
    expect(document.getSnapshot().workflow!.jobs.test.steps![0].argv).toEqual(['echo', 'done']);
    document.renameStep('test', 0, 'actual_step');
    expect(document.getSnapshot().diagnostics).toEqual([]);
    expect(document.getSnapshot().workflow!.jobs.test.steps![0]).toEqual({ argv: ['echo', 'done'], id: 'actual_step' });
    for (const comment of ['preserve skipped step', 'inline skipped comment', 'trailing skipped comment']) expect(document.getSnapshot().source).toContain(comment);
  });
  it('materializes editable zero-value callable structs and jobs', () => {
    const source = 'on: {workflow_call: {inputs: {a: null}, secrets: {s: null}, outputs: {o: null}}}\njobs: {test: null}';
    const document = new WorkflowDocument({ source });
    expect(document.getSnapshot().source).toBe(source);
    expect(document.getSnapshot().stale).toBe(false);
    expect(document.getSnapshot().diagnostics).toContainEqual(expect.objectContaining({ code: 'steps', path: ['jobs', 'test', 'steps'] }));
    document.setField(['on', 'workflow_call', 'inputs', 'a', 'required'], true);
    document.setField(['on', 'workflow_call', 'secrets', 's', 'required'], true);
    document.setField(['on', 'workflow_call', 'outputs', 'o', 'value'], 'value');
    document.addStep('test');
    expect(document.getSnapshot().diagnostics).toEqual([]);
    expect((document.getSnapshot().workflow!.on!.workflow_call as any).inputs.a.required).toBe(true);
  });
  it('recovers editable steps from null typed slices', () => {
    const document = new WorkflowDocument({ source: 'jobs: {test: {steps: null}}' });
    expect(document.getSnapshot().stale).toBe(false);
    document.addStep('test');
    expect(document.getSnapshot().diagnostics).toEqual([]);
    expect(document.getSnapshot().workflow!.jobs.test.steps).toHaveLength(1);
  });
  it('does not reinterpret null values inside untyped action parameters', () => {
    const result = parseWorkflow('jobs: {test: {steps: [{uses: agent/claude, with: {prompt: [one, null, two]}}]}}');
    expect(result.workflow!.jobs.test.steps![0].with!.prompt).toEqual(['one', null, 'two']);
  });
});

describe('typed null scalar decoding', () => {
  it('returns Go zero values with accurate types and preserves runtime meaning through edits', () => {
    const source = 'name: null\njobs: {build: {needs: null, continue-on-error: null, outputs: {result: null}, repeat: null, steps: [{id: null, run: null, uses: agent/claude, with: {prompt: ok}}]}}';
    const document = new WorkflowDocument({ source });
    const workflow = document.getSnapshot().workflow!;
    expect(workflow.name).toBe(''); expect(workflow.jobs.build.needs).toEqual([]);
    expect(workflow.jobs.build['continue-on-error']).toBe(false); expect(workflow.jobs.build.outputs!.result).toBe('');
    expect(workflow.jobs.build.steps![0].id).toBe(''); expect(workflow.jobs.build.steps![0].run).toBe('');
    expect(workflow.jobs.build.repeat).toBeNull(); expect(document.getSnapshot().source).toBe(source);
    expect(document.getSnapshot().diagnostics).toEqual([]);
    document.setField(['name'], 'Changed');
    expect(document.getSnapshot().workflow!.jobs).toEqual(workflow.jobs);
  });
});

describe('tagged legacy boolean round trips', () => {
  it('keeps typed boolean meaning when legacy spelling was explicitly tagged as a string', () => {
    const source = 'on: {workflow_call: {inputs: {task: {required: !!str YES}}}}\njobs: {build: {continue-on-error: !!str NO, steps: [{argv: [echo]}]}}';
    const document = new WorkflowDocument({ source });
    const before = document.getSnapshot().workflow;
    expect(document.getSnapshot().diagnostics).toEqual([]); expect(document.getSnapshot().source).toBe(source);
    document.setField(['name'], 'Changed');
    expect(document.getSnapshot().diagnostics).toEqual([]);
    expect(document.getSnapshot().workflow!.jobs).toEqual(before!.jobs);
    expect(document.getSnapshot().workflow!.on).toEqual(before!.on);
  });
});
