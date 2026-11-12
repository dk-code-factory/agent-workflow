import type { Path, Step } from './types';
import { controllerParameters, stepAction } from './validation';

// Paths are relative to the step. Editing individual nodes keeps comments and
// formatting on compatible configuration intact.
export function stepConversion(step: Step, kind: string) {
  const uses = stepAction(step);
  const command = kind === 'argv' || kind === 'shell';
  const agent = kind === 'agent/run';
  const conversation = kind === 'conversation';
  const block = kind === 'parallel' || kind === 'sequence';
  const remove: Path[] = [];
  const set: { path: Path; value: unknown }[] = [];
  const drop = (key: string) => { if (Object.hasOwn(step, key)) remove.push([key]); };
  for (const key of ['conversation', 'start', 'finishBy', 'maxTurns', 'maxMessages']) if (!conversation) drop(key);
  for (const key of ['onResult', 'onComplete', 'onIncomplete']) if (!agent) drop(key);
  if (!agent && !command) for (const key of ['onError', 'onFailure', 'finally', 'handlerTimeout']) drop(key);
  for (const key of ['parallel', 'sequence']) if (key !== kind) drop(key);
  if (block) {
    for (const key of ['uses', 'agent', 'prompt', 'with', 'argv', 'run', 'shell', 'timeout', 'working-directory', 'context', 'continue-on-error']) drop(key);
    if (!Object.hasOwn(step, kind)) set.push({ path: [kind], value: [{ argv: ['echo', 'Hello'] }] });
  } else if (conversation) {
    for (const key of ['uses', 'agent', 'argv', 'run', 'shell', 'working-directory', 'context']) drop(key);
    if (!step.conversation) {
      set.push({ path: ['conversation'], value: ['builder', 'reviewer'] }, { path: ['finishBy'], value: 'reviewer' });
      if (step.prompt === undefined) set.push({ path: ['prompt'], value: String(step.with?.prompt ?? 'Complete the task together.') });
      for (const key of Object.keys(step.with ?? {})) if (!['model', 'workspace', 'output'].includes(key)) remove.push(['with', key]);
    }
  } else if (command) {
    for (const key of ['uses', 'agent', 'prompt', 'with']) drop(key);
    for (const key of kind === 'argv' ? ['run', 'shell'] : ['argv']) drop(key);
    if (kind === 'argv' && !step.argv?.length) set.push({ path: ['argv'], value: ['echo', 'Hello'] });
    if (kind === 'shell') {
      if (!step.run) set.push({ path: ['run'], value: 'echo Hello' });
      if (!step.shell) set.push({ path: ['shell'], value: 'sh' });
    }
  } else {
    for (const key of ['agent', 'prompt', 'argv', 'run', 'shell', 'timeout', 'working-directory']) drop(key);
    set.push({ path: ['uses'], value: kind });
    if (uses !== kind && !(agent && step.conversation)) {
      const before = controllerParameters[uses ?? ''] ?? [];
      const after = controllerParameters[kind] ?? [];
      for (const key of Object.keys(step.with ?? {})) if (!before.includes(key) || !after.includes(key)) remove.push(['with', key]);
    }
    if (agent) {
      if (!step.with?.agent) set.push({ path: ['with', 'agent'], value: '' });
      if (!step.with?.prompt || remove.some(path => path[0] === 'with' && path[1] === 'prompt')) set.push({ path: ['with', 'prompt'], value: step.prompt ?? '' });
    }
  }
  return { remove, set };
}

export function populated(value: unknown): boolean {
  return value !== undefined && value !== null && value !== '' &&
    (typeof value !== 'object' || Object.keys(value).length > 0);
}

export function stepConversionImpact(step: Step, kind: string) {
  return stepConversion(step, kind).remove.filter(path => {
    const value = path.length === 1 ? step[String(path[0])] : step.with?.[String(path[1])];
    return populated(value);
  }).map(path => path.join('.'));
}
