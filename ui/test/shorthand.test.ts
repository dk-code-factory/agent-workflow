import { describe, expect, it } from 'vitest';
import { WorkflowDocument } from '../src/document';
import { parseWorkflow } from '../src/validation';

describe('native agent tasks', () => {
  it.each(['agent: reviewer, prompt: Review','uses: agent/reviewer, with: {prompt: Review}','uses: agent/run, with: {prompt: Review}','uses: agent/run, prompt: Review, with: {agent: reviewer.toml}'])('rejects removed or incomplete syntax: %s', step => {
    expect(parseWorkflow(`jobs: {review: {steps: [{${step}}]}}`).diagnostics.some(d => d.severity === 'error')).toBe(true);
  });
  it('retains typed input and six hooks through editing and history', () => {
    const source=`jobs:
  review:
    steps:
      - uses: agent/run
        with:
          agent: reviewer.toml
          prompt: Review
          input: {count: 7, enabled: true}
          output: {format: json, schema: {type: object, required: [complete], properties: {complete: {type: boolean}}}}
        onResult: [{argv: [python3, verify.py]}]
        onComplete: [{argv: [echo, accepted]}]
        onIncomplete: [{argv: [echo, feedback]}]
        onError: [{argv: [echo, recover]}]
        onFailure: [{argv: [echo, report]}]
        finally: [{argv: [echo, finalize]}]
`;
    const doc=new WorkflowDocument({source});
    const hooks = {
      onResult: [{argv: ['python3', 'verify.py']}], onComplete: [{argv: ['echo', 'accepted']}],
      onIncomplete: [{argv: ['echo', 'feedback']}], onError: [{argv: ['echo', 'recover']}],
      onFailure: [{argv: ['echo', 'report']}], finally: [{argv: ['echo', 'finalize']}],
    };
    expect(doc.getSnapshot().diagnostics.filter(d=>d.severity==='error')).toEqual([]);
    doc.setField(['jobs','review','steps',0,'with','prompt'],'Review carefully');
    const edited=doc.getSnapshot().source;
    expect(doc.getSnapshot().workflow!.jobs.review.steps![0]).toMatchObject(hooks);
    expect(doc.getSnapshot().workflow!.jobs.review.steps![0].with!.input).toEqual({count:7,enabled:true});
    doc.undo(); expect(doc.getSnapshot().source).toBe(source);doc.redo();expect(doc.getSnapshot().source).toBe(edited);
    expect(doc.getSnapshot().workflow!.jobs.review.steps![0]).toMatchObject(hooks);
    expect(parseWorkflow(edited).diagnostics.filter(d=>d.severity==='error')).toEqual([]);
    doc.setStepKind('review',0,'argv');
    const converted = doc.getSnapshot().workflow!.jobs.review.steps![0];
    for (const hook of ['onResult', 'onComplete', 'onIncomplete']) expect(converted[hook]).toBeUndefined();
    expect(converted).toMatchObject({onError: hooks.onError, onFailure: hooks.onFailure, finally: hooks.finally});
    doc.undo();expect(doc.getSnapshot().source).toBe(edited);
    expect(doc.getSnapshot().workflow!.jobs.review.steps![0]).toMatchObject(hooks);
  });
});
