import { describe, expect, it } from 'vitest';
import { parseWorkflow } from '../src/validation';

describe('admitted agent definition selections', () => {
  const parse = (agent: string) => parseWorkflow(`on: {workflow_call: {inputs: {role: {type: string}}}}
jobs: {review: {steps: [{uses: agent/run, with: {agent: ${agent}, prompt: Review}}]}}`);
  it.each([
    "{select: review, definitions: {review: reviewer.toml}}",
    "{select: '${{ inputs.role }}', definitions: {review: reviewer.toml, fix: fixer.toml}}",
  ])('accepts literal definitions with a named selector: %s', agent => {
    expect(parse(agent).diagnostics.filter(d => d.severity === 'error')).toEqual([]);
  });
  it.each([
    '{select: review, definitions: {}}',
    "{select: review, definitions: {review: '${{ inputs.role }}'}}",
    "{select: review, definitions: '${{ inputs.role }}'}",
    '{select: missing, definitions: {review: reviewer.toml}}',
    '{select: 1, definitions: {review: reviewer.toml}}',
    '{select: review, definitions: {review: reviewer.toml}, extra: true}',
    '{select: review, definitions: {' + Array.from({ length: 33 }, (_, i) => `role${i}: reviewer.toml`).join(', ') + '}}',
  ])('rejects malformed or dynamic catalogs: %s', agent => {
    expect(parse(agent).diagnostics).toContainEqual(expect.objectContaining({ code: 'agent-definition', severity: 'error' }));
  });
});
