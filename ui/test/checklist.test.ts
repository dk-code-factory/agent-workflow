import { expect, it } from 'vitest';
import { parseWorkflow } from '../src/validation';

it.each([
  '{max-retries: 0, stages: [Verify]}',
  '{max-retries: 0, stages: [!!binary 77u/]}',
  "{max-retries: 0, stages: [!!binary VmVyaWZ5, {prompt: !!binary VmVyaWZ5}, {argv: [!!binary ZWNobw==, !!binary b2s=, !!binary w6k=, !!binary MjAyNi0wMS0wMQ==]}, {run: !!binary dHJ1ZQ==, shell: !!binary c2g=, timeout: !!binary MW0=, working-directory: !!binary Lg==}]}",

  "{max-retries: 0, stages: [{argv: [echo, '2026-01-01']}]}",
  '{max-retries: 4, stages: [{prompt: Verify, max-retries: 1}, {argv: [go, test, ./...], timeout: 5m}, {shell: sh, run: git diff --check}]}',
])('admits checklist prompt and command stages: %s', checklist => {
  expect(parseWorkflow(`jobs: {build: {steps: [{uses: agent/run, with: {agent: developer.toml, prompt: Work, checklist: ${checklist}}}]}}`).diagnostics.filter(d => d.severity === 'error')).toEqual([]);
});

it.each([
  '{max-retries: 0, stages: [{argv: [echo, 2026-01-01]}]}',
  '{max-retries: 0, stages: [!!binary /w==]}',
  '{max-retries: 0, stages: [{argv: [echo, !!binary /w==]}]}',
  '{max-retries: 0, stages: [{argv: [echo, !!binary b2s]}]}',
  '{max-retries: 0, stages: ["\\u0085"]}',
  'null', '{}', '{max-retries: -1, stages: [Verify]}', '{max-retries: 1.5, stages: [Verify]}',
  "{max-retries: '2', stages: [Verify]}", '{max-retries: 1, stages: []}',
  '{max-retries: 1, stages: [{prompt: Verify, argv: [echo]}]}',
  '{max-retries: 1, stages: [{run: echo}]}', '{max-retries: 1, stages: [{argv: [echo], timeout: 0s}]}',
  '{max-retries: 1, stages: [{prompt: Verify, max-retries: 101}]}',
])('rejects malformed checklists: %s', checklist => {
  expect(parseWorkflow(`jobs: {build: {steps: [{uses: agent/run, with: {agent: developer.toml, prompt: Work, checklist: ${checklist}}}]}}`).diagnostics.some(d => d.code === 'checklist' && d.severity === 'error')).toBe(true);
});

it('preserves decoded checklist binary strings through serialization', () => {
  const source = 'jobs: {build: {steps: [{uses: agent/run, with: {agent: developer.toml, prompt: Work, checklist: {max-retries: 0, stages: [!!binary VmVyaWZ5, {argv: [echo, !!binary w6k=, !!binary MjAyNi0wMS0wMQ==]}]}}}]}}';
  const parsed = parseWorkflow(source);
  expect(parsed.workflow?.jobs.build.steps?.[0].with?.checklist).toEqual({ 'max-retries': 0, stages: ['Verify', { argv: ['echo', 'é', '2026-01-01'] }] });
  const roundtrip = parseWorkflow(String(parsed.doc));
  expect(roundtrip.diagnostics.filter(d => d.severity === 'error')).toEqual([]);
  expect(roundtrip.workflow?.jobs.build.steps?.[0].with?.checklist).toEqual(parsed.workflow?.jobs.build.steps?.[0].with?.checklist);
});
