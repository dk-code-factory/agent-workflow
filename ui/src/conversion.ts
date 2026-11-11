import type { Path, Step } from './types';
import { controllerParameters, stepAction } from './validation';

// Paths are relative to the step. Editing individual nodes keeps comments and
// formatting on compatible configuration intact.
export function stepConversion(step: Step, kind: string) {
  const uses = stepAction(step);
  const shorthand = Object.hasOwn(step, 'agent');
  const command = kind === 'argv' || kind === 'shell';
  const remove: Path[] = [];
  if (!command && !kind.startsWith('docker://')) for (const key of ['protocol', 'effect', 'input-schema', 'output-schema', 'input', 'env']) if (Object.hasOwn(step, key)) remove.push([key]);
  const set: { path: Path; value: unknown }[] = [];
  const conversation = kind === 'conversation';
  for (const key of ['conversation', 'start', 'finishBy', 'maxTurns', 'maxMessages']) if (!conversation && Object.hasOwn(step, key)) remove.push([key]);
  const block = kind === 'parallel' || kind === 'sequence';
  if (!command && !kind.startsWith('agent/')) for (const key of ['onError', 'onFailure', 'finally', 'handlerTimeout']) {
    if (Object.hasOwn(step, key)) remove.push([key]);
  }
  for (const key of ['parallel', 'sequence']) if (key !== kind && Object.hasOwn(step, key)) remove.push([key]);
  if (conversation) {
    for (const key of ['uses', 'agent', 'argv', 'run', 'shell', 'working-directory', 'context']) if (Object.hasOwn(step, key)) remove.push([key]);
    if (!step.conversation) {
      set.push({ path: ['conversation'], value: ['builder', 'reviewer'] }, { path: ['finishBy'], value: 'reviewer' });
      if (step.prompt === undefined) set.push({ path: ['prompt'], value: String(step.with?.prompt ?? 'Complete the task together.') });
      for (const key of Object.keys(step.with ?? {})) if (!['model', 'workspace', 'output'].includes(key)) remove.push(['with', key]);
    }
    return { remove, set };
  }
  if (block) {
    for (const key of ['uses', 'agent', 'prompt', 'with', 'argv', 'run', 'shell', 'timeout', 'working-directory', 'context', 'continue-on-error']) if (Object.hasOwn(step, key)) remove.push([key]);
    if (!Object.hasOwn(step, kind)) set.push({ path: [kind], value: [{ argv: ['echo', 'Hello'] }] });
    return { remove, set };
  }
  for (const key of command ? ['uses', 'agent', 'prompt', 'with'] : kind.startsWith('docker://') && uses?.startsWith('docker://') ? ['argv', 'run', 'shell'] : ['argv', 'run', 'shell', 'timeout', 'working-directory']) {
    if (Object.hasOwn(step, key)) remove.push([key]);
  }
  if (command) {
    for (const key of kind === 'argv' ? ['run', 'shell'] : ['argv']) {
      if (Object.hasOwn(step, key)) remove.push([key]);
    }
    if (kind === 'argv' && !step.argv?.length) set.push({ path: ['argv'], value: ['echo', 'Hello'] });
    if (kind === 'shell') {
      if (!step.run) set.push({ path: ['run'], value: 'echo Hello' });
      if (!step.shell) set.push({ path: ['shell'], value: 'sh' });
    }
  } else {
    if (shorthand && kind.startsWith('agent/')) set.push({ path: ['agent'], value: kind.slice(6) });
    else {
      set.push({ path: ['uses'], value: kind });
      for (const key of ['agent', 'prompt']) if (Object.hasOwn(step, key)) remove.push([key]);
    }
    if (uses !== kind && !(uses?.startsWith('agent/') && kind.startsWith('agent/'))) {
      const before = uses?.startsWith('docker://') ? ['entrypoint', 'args'] : Object.hasOwn(controllerParameters, uses ?? '') ? controllerParameters[uses!] : [];
      const after = kind.startsWith('docker://') ? ['entrypoint', 'args'] : Object.hasOwn(controllerParameters, kind) ? controllerParameters[kind] : [];
      for (const key of Object.keys(step.with ?? {})) {
        if (!before.includes(key) || !after.includes(key)) remove.push(['with', key]);
      }
    }
    if (step.conversation && kind.startsWith('agent/')) {
      set.push({ path: ['agent'], value: kind.slice(6) });
      const usesIndex = set.findIndex(entry => entry.path[0] === 'uses');
      if (usesIndex >= 0) set.splice(usesIndex, 1);
      const promptIndex = remove.findIndex(path => path[0] === 'prompt');
      if (promptIndex >= 0) remove.splice(promptIndex, 1);
      for (let index = remove.length - 1; index >= 0; index--) if (remove[index][0] === 'with' && ['model', 'workspace', 'output'].includes(String(remove[index][1]))) remove.splice(index, 1);
    }
    if (kind.startsWith('agent/') && !step.conversation && step.prompt === undefined && (step.with?.prompt === undefined || remove.some(path => path[0] === 'with' && path[1] === 'prompt'))) set.push({ path: shorthand ? ['prompt'] : ['with', 'prompt'], value: '' });
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
