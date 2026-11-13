import { describe, expect, it } from 'vitest';
import { stringify } from 'yaml';
import { WorkflowDocument } from '../src/document';
import { stepConversionImpact } from '../src/conversion';
import { expressionSuggestions } from '../src/source';
import { parseWorkflow } from '../src/validation';
import { hasExpressionReferences, isExpressionPath } from '../src/expressions';

const source = `# preserve handlers
jobs:
  build:
    steps:
      - id: earlier
        uses: agent/run
        with:
          agent: claude.toml
          prompt: Prepare
      - id: task
        handlerTimeout: 45s
        onError:
          - id: earlier
            uses: agent/run
            with:
              agent: codex.toml
              prompt: \${{ failed.prompt }} \${{ steps.earlier.text }}
          - uses: agent/run
            with:
              agent: claude.toml
              prompt: \${{ steps.earlier.text }} \${{ error.message }}
        onFailure:
          - argv: [ echo, '\${{ error.class }}' ]
        finally:
          - argv: [ echo, done ]
        uses: agent/run
        with:
          agent: claude.toml
          prompt: Work
`;

describe('step lifecycle handler authoring', () => {
  it('preserves hooks through focused edits, conversion and undo', () => {
    const doc = new WorkflowDocument({ source });
    expect(doc.getSnapshot().diagnostics).toEqual([]);
    const original = doc.getSnapshot().workflow!.jobs.build.steps![1];
    doc.setField(['name'], 'Handlers');
    doc.setField(['jobs','build','steps',1,'with','agent'],'codex.toml');
    expect(doc.getSnapshot().workflow!.jobs.build.steps![1]).toEqual({ ...original, with: {...original.with, agent: 'codex.toml'} });
    expect(doc.getSnapshot().source).toContain('# preserve handlers');
    doc.undo(); doc.undo();
    expect(doc.getSnapshot().source).toBe(source);
  });

  it('rewrites inherited references without changing a shadowing hook sibling', () => {
    const doc = new WorkflowDocument({ source });
    doc.renameStep('build', 0, 'prepared');
    const fallback = doc.getSnapshot().workflow!.jobs.build.steps![1].onError!;
    expect(fallback[0].with!.prompt).toContain('steps.prepared.text');
    expect(fallback[1].with!.prompt).toContain('steps.earlier.text');
    expect(doc.getSnapshot().diagnostics).toEqual([]);
  });

  it('tracks named schemas consumed by nested handlers without rewriting shadowed consumers', () => {
    const original = stringify({ schemas: { Shared: { type: 'string', enum: ['${{ steps.earlier.text }}'] } }, jobs: { build: { steps: [
      {id: 'earlier', uses: 'agent/run', with: {agent: "claude.toml", prompt: 'Prepare'}},
      {onError: [
        {uses: 'agent/run', with: {agent: "codex.toml", output: { schema: 'Shared' }, prompt: 'Inherited'}},
        {id: 'earlier', uses: 'agent/run', with: {agent: "codex.toml", prompt: 'Local'}},
        {uses: 'agent/run', with: {agent: "claude.toml", output: { schema: 'Shared' }, prompt: 'Shadowed'}},
      ], finally: [{uses: 'agent/run', with: {agent: "claude.toml", output: { schema: 'Shared' }, prompt: 'Finalize'}}], uses: 'agent/run', with: {agent: "claude.toml", prompt: 'Work'}},
    ] } } });
    const doc = new WorkflowDocument({ source: original });
    expect(doc.deletionImpact('build', 0)).toContainEqual(['schemas', 'Shared', 'enum', 0]);
    expect(() => doc.deleteStep('build', 0)).toThrow('references');
    doc.renameStep('build', 0, 'prepared');
    const edited = doc.getSnapshot();
    expect(edited.workflow!.schemas!.Shared.enum).toEqual(['${{ steps.earlier.text }}']);
    expect(edited.workflow!.schemas!.Shared_build.enum).toEqual(['${{ steps.prepared.text }}']);
    const task = edited.workflow!.jobs.build.steps![1];
    expect((task.onError![0].with!.output as any).schema).toBe('Shared_build');
    expect((task.onError![2].with!.output as any).schema).toBe('Shared');
    expect((task.finally![0].with!.output as any).schema).toBe('Shared_build');
    expect(edited.diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    doc.undo(); expect(doc.getSnapshot().source).toBe(original);
    doc.redo(); expect(doc.getSnapshot().source).toBe(edited.source);
    doc.deleteStep('build', 0, true);
    expect(doc.getSnapshot().diagnostics).toContainEqual(expect.objectContaining({ code: 'expression-reference', path: ['schemas', 'Shared_build', 'enum', 0] }));
  });

  it.each(['prompt', 'schema'])('rejects rename capture by a sibling matching the new ID in %s', kind => {
    const original = stringify({ schemas: { Shared: { type: 'string', enum: ['${{ steps.earlier.text }}'] } }, jobs: { build: { steps: [
      {id: 'earlier', uses: 'agent/run', with: {agent: "claude.toml", prompt: 'Prepare'}},
      {finally: [
        {id: 'prepared', uses: 'agent/run', with: {agent: "codex.toml", prompt: 'Local'}},
        kind === 'prompt' ? {uses: 'agent/run', with: {agent: "claude.toml", prompt: '${{ steps.earlier.text }}'}} : {uses: 'agent/run', with: {agent: "claude.toml", output: { schema: 'Shared' }, prompt: 'Inherited'}},
      ], uses: 'agent/run', with: {agent: "claude.toml", prompt: 'Work'}},
    ] } } });
    const doc = new WorkflowDocument({ source: original });
    expect(doc.getSnapshot().diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    expect(() => doc.renameStep('build', 0, 'prepared')).toThrow('shadowed by a handler sibling');
    expect(doc.getSnapshot().source).toBe(original);
    expect(doc.getSnapshot().canUndo).toBe(false);
    doc.renameStep('build', 0, 'ready');
    expect(doc.getSnapshot().source).toContain('steps.ready.text');
    expect(doc.getSnapshot().diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    doc.undo(); expect(doc.getSnapshot().source).toBe(original);
  });

  it.each(['earlier', 'task', 'fallback', 'later', 'missing'])('validates named handler schemas against the local scope: %s', reference => {
    const yaml = stringify({ schemas: { Result: { type: 'string', enum: [`\${{ steps.${reference}.text }}`] } }, jobs: { build: { steps: [
      {id: 'earlier', uses: 'agent/run', with: {agent: "claude.toml", prompt: 'Prepare'}},
      {id: 'task', onError: [
        {id: 'fallback', uses: 'agent/run', with: {agent: "claude.toml", output: { schema: 'Result' }, prompt: 'Fallback'}},
        {id: 'later', uses: 'agent/run', with: {agent: "claude.toml", prompt: 'Later'}},
      ], uses: 'agent/run', with: {agent: "claude.toml", prompt: 'Work'}},
    ] } } });
    expect(parseWorkflow(yaml).diagnostics.some(d => d.code === 'expression-reference' && d.path.join('.') === 'schemas.Result.enum.0')).toBe(reference !== 'earlier');
  });

  it('discloses unsupported handlers when converting to a controller action', () => {
    const doc = new WorkflowDocument({ source });
    expect(stepConversionImpact(doc.getSnapshot().workflow!.jobs.build.steps![1], 'workspace/create')).toEqual(expect.arrayContaining(['onError', 'onFailure', 'finally', 'handlerTimeout']));
    doc.setStepKind('build', 1, 'workspace/create');
    expect(doc.getSnapshot().workflow!.jobs.build.steps![1].onError).toBeUndefined();
    doc.undo(); expect(doc.getSnapshot().source).toBe(source);
  });

  it('suggests authenticated failure context only inside hooks and uses preceding sibling scope', () => {
    const w = parseWorkflow(source).workflow!;
    const path = ['jobs', 'build', 'steps', 1, 'onError', 1, 'with', 'prompt'];
    expect(expressionSuggestions(w, 'build', 1, path)).toEqual(expect.arrayContaining(['failed.prompt', 'error.message', 'steps.earlier.text']));
    expect(expressionSuggestions(w, 'build', 1)).not.toContain('failed.prompt');
    expect(expressionSuggestions(w, 'build', 1, ['jobs', 'build', 'steps', 1, 'finally', 0, 'argv', 1])).not.toContain('steps.step_2.text');
    expect(hasExpressionReferences(path)).toBe(true);
    expect(isExpressionPath(['jobs', 'build', 'steps', 1, 'onError', 0, 'if'])).toBe(true);
    expect(hasExpressionReferences(['jobs', 'build', 'steps', 1, 'with', 'onError', 0, 'prompt'])).toBe(true);
  });

  it.each([
    ['job hooks', 'jobs: {build: {onError: [], steps: [{argv: [echo]}]}}', 'unknown-field'],
    ['workflow hooks', 'finally: []\njobs: {build: {steps: [{argv: [echo]}]}}', 'unknown-field'],
    ['invalid nested action', 'jobs: {build: {steps: [{argv: [echo], onError: [{uses: unknown}]}]}}', 'action'],
    ['invalid duration', 'jobs: {build: {steps: [{argv: [echo], handlerTimeout: 6m}]}}', 'handler-timeout'],
    ['orphan timeout', 'jobs: {build: {steps: [{argv: [echo], handlerTimeout: 45s}]}}', 'handler-timeout'],
    ['wrong hook shape', 'jobs: {build: {steps: [{argv: [echo], finally: nope}]}}', 'shape'],
    ['context outside hook', `jobs: { build: { steps: [ { uses: agent/run, with: { agent: claude.toml, prompt: "\${{ failed.prompt }}" } } ] } }
`, 'handler-context'],
    ['invalid context field', source.replace('failed.prompt', 'failed.credentials'), 'handler-context'],
    ['sibling hook invisibility', source.replace('argv: [ echo, done ]', 'argv: [echo, "${{ steps.step_2.text }}"]'), 'expression-reference'],
  ])('diagnoses %s', (_name, yaml, code) => {
    expect(parseWorkflow(yaml).diagnostics.some(d => d.code === code && d.severity === 'error')).toBe(true);
  });

  it.each(['failed.id', 'failed.uses', 'failed.prompt', 'error.message', 'error.class'])('rejects output traversal through scalar handler context %s', reference => {
    for (const suffix of ['.outputs.foo', '.outputs.', '.outputs.foo.bar']) {
      const yaml = source.replace('failed.prompt', reference + suffix);
      expect(parseWorkflow(yaml).diagnostics).toContainEqual(expect.objectContaining({
        code: 'handler-context', severity: 'error',
        message: `Unknown ${reference.split('.')[0]} context field "${reference.split('.')[1]}${suffix}"`,
      }));
    }
    expect(parseWorkflow(source.replace('failed.prompt', reference)).diagnostics).toEqual([]);
  });

  it('keeps handler scopes and contracts inside structural branches through duplicate and undo', () => {
    const original = stringify({ schemas: { Shared: { type: 'string', enum: ['${{ steps.seed.text }}'] } }, jobs: { build: { steps: [
      { id: 'group', parallel: [
        { sequence: [
          {id: 'seed', uses: 'agent/run', with: {agent: "claude.toml", prompt: 'Seed'}},
          {id: 'work', onError: [
            {uses: 'agent/run', with: {agent: "codex.toml", output: { schema: 'Shared' }, prompt: '${{ failed.prompt }} ${{ steps.seed.text }}'}},
            {id: 'seed', uses: 'agent/run', with: {agent: "codex.toml", prompt: 'Local'}},
            {uses: 'agent/run', with: {agent: "claude.toml", output: { schema: 'Shared' }, prompt: '${{ steps.seed.text }}'}},
          ], uses: 'agent/run', with: {agent: "claude.toml", prompt: 'Work'}},
        ] },
        {id: 'sibling', uses: 'agent/run', with: {agent: "claude.toml", prompt: 'Sibling'}},
      ] },
    ] } } });
    const doc = new WorkflowDocument({ source: original });
    expect(doc.getSnapshot().diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    const owner = ['jobs', 'build', 'steps', 0, 'parallel', 0, 'sequence', 1];
    const suggestions = expressionSuggestions(doc.getSnapshot().workflow, 'build', 0, [...owner, 'onError', 0, 'with', 'prompt']);
    expect(suggestions).toEqual(expect.arrayContaining(['failed.prompt', 'steps.seed.text']));
    expect(suggestions).not.toContain('steps.sibling.text');
    expect(expressionSuggestions(doc.getSnapshot().workflow, 'build', 0, [...owner, 'with', 'prompt'])).not.toContain('failed.prompt');
    doc.duplicateStep('build', 0);
    const copy = doc.getSnapshot().workflow!.jobs.build.steps![1].parallel![0].sequence![1].onError!;
    expect(copy[0].with!.prompt).toContain('steps.seed_copy.text');
    expect(copy[2].with!.prompt).toBe('${{ steps.seed.text }}');
    expect(copy[0].with!.output).toEqual({ schema: 'Shared_copy' });
    expect(copy[2].with!.output).toEqual({ schema: 'Shared' });
    expect(doc.getSnapshot().workflow!.schemas!.Shared_copy.enum).toEqual(['${{ steps.seed_copy.text }}']);
    expect(doc.getSnapshot().diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    doc.undo(); expect(doc.getSnapshot().source).toBe(original);
    for (const reference of ['steps.sibling.text', 'steps.work.text']) {
      const invalid = original.replace('failed.prompt }} ${{ steps.seed.text', 'failed.prompt }} ${{ ' + reference);
      expect(parseWorkflow(invalid).diagnostics.some(d => d.code === 'expression-reference')).toBe(true);
    }
  });

  it.each(['onError', 'onFailure', 'finally'] as const)('avoids generated IDs captured by direct and nested %s siblings', kind => {
    const original = stringify({ schemas: { Shared: { type: 'string', enum: ['${{ steps.seed.text }}'] } }, jobs: { build: { steps: [
      { id: 'group', sequence: [
        {id: 'seed', uses: 'agent/run', with: {agent: "claude.toml", prompt: 'Seed'}},
        {id: 'work', [kind]: [
          {id: 'seed_copy', uses: 'agent/run', with: {agent: "codex.toml", prompt: 'Local copy name'}},
          {finally: [
            {id: 'seed_copy_2', uses: 'agent/run', with: {agent: "codex.toml", prompt: 'Nested copy name'}},
            {uses: 'agent/run', with: {agent: "claude.toml", output: { schema: 'Shared' }, prompt: '${{ steps.seed.text }} ${{ steps.seed_copy.text }} ${{ steps.seed_copy_2.text }}'}},
          ], uses: 'agent/run', with: {agent: "claude.toml", output: { schema: 'Shared' }, prompt: '${{ steps.seed.text }}'}},
          {id: 'seed', uses: 'agent/run', with: {agent: "codex.toml", prompt: 'Local source name'}},
          {uses: 'agent/run', with: {agent: "claude.toml", output: { schema: 'Shared' }, prompt: '${{ steps.seed.text }}'}},
        ], uses: 'agent/run', with: {agent: "claude.toml", prompt: 'Work'}},
      ] },
    ] } } });
    const doc = new WorkflowDocument({ source: original });
    expect(doc.getSnapshot().diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    doc.duplicateStep('build', 0);
    const edited = doc.getSnapshot();
    const sequence = edited.workflow!.jobs.build.steps![1].sequence!;
    const handlers = sequence[1][kind]!;
    expect(sequence[0].id).toBe('seed_copy_3');
    expect(handlers[0].id).toBe('seed_copy');
    expect(handlers[1].with!.prompt).toBe('${{ steps.seed_copy_3.text }}');
    expect(handlers[1].with!.output).toEqual({ schema: 'Shared_copy' });
    expect(handlers[1].finally![0].id).toBe('seed_copy_2');
    expect(handlers[1].finally![1].with!.prompt).toBe('${{ steps.seed_copy_3.text }} ${{ steps.seed_copy.text }} ${{ steps.seed_copy_2.text }}');
    expect(handlers[1].finally![1].with!.output).toEqual({ schema: 'Shared_copy' });
    expect(handlers[3].with!.prompt).toBe('${{ steps.seed.text }}');
    expect(handlers[3].with!.output).toEqual({ schema: 'Shared' });
    expect(edited.workflow!.schemas!.Shared_copy.enum).toEqual(['${{ steps.seed_copy_3.text }}']);
    expect(edited.workflow!.schemas!.Shared.enum).toEqual(['${{ steps.seed.text }}']);
    expect(edited.diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    expect(parseWorkflow(edited.source).workflow).toEqual(edited.workflow);
    doc.undo(); expect(doc.getSnapshot().source).toBe(original);
    doc.redo(); expect(doc.getSnapshot().source).toBe(edited.source);
  });

  it.each(['parallel', 'sequence'])('rejects structural %s blocks in handler lists and hooks on blocks', kind => {
    const leaf = { argv: ['echo'] };
    expect(parseWorkflow(stringify({ jobs: { build: { steps: [{ [kind]: [{ argv: ["echo"] }], finally: [{ argv: ["echo"] }] }] } } })).diagnostics.some(d => d.code === 'block')).toBe(true);
    expect(parseWorkflow(stringify({ jobs: { build: { steps: [{ argv: ["echo"], onError: [{ [kind]: [{ argv: ["echo"] }] }] }] } } })).diagnostics.some(d => d.code === 'handler-action')).toBe(true);
    expect(stepConversionImpact({ ...leaf, finally: [leaf], handlerTimeout: '30s' }, kind)).toEqual(expect.arrayContaining(['finally', 'handlerTimeout']));
  });

  it('bounds nesting and handler count', () => {
    let nested: object = { argv: ['echo'] };
    for (let i = 0; i < 9; i++) nested = { argv: ['echo'], onError: [nested] };
    expect(parseWorkflow(stringify({ jobs: { build: { steps: [nested] } } })).diagnostics.some(d => d.code === 'handler-depth')).toBe(true);
    expect(parseWorkflow(stringify({ jobs: { build: { steps: [{ argv: ['echo'], finally: Array.from({ length: 101 }, () => ({ argv: ['echo'] })) }] } } })).diagnostics.some(d => d.code === 'handler-count')).toBe(true);
  });
});
