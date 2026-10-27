import type { Path, Step } from './types';
import { controllerParameters } from './validation';

// Paths are relative to the step. Editing individual nodes keeps comments and
// formatting on compatible configuration intact.
export function stepConversion(step: Step, kind: string) {
  const command = kind === 'argv' || kind === 'shell';
  const remove: Path[] = [];
  const set: { path: Path; value: unknown }[] = [];
  for (const key of command ? ['uses', 'with'] : ['argv', 'run', 'shell', 'timeout', 'working-directory']) {
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
    set.push({ path: ['uses'], value: kind });
    if (step.uses !== kind && !(step.uses?.startsWith('agent/') && kind.startsWith('agent/'))) {
      const before = Object.hasOwn(controllerParameters, step.uses ?? '') ? controllerParameters[step.uses!] : [];
      const after = Object.hasOwn(controllerParameters, kind) ? controllerParameters[kind] : [];
      for (const key of Object.keys(step.with ?? {})) {
        if (!before.includes(key) || !after.includes(key)) remove.push(['with', key]);
      }
    }
    if (kind.startsWith('agent/') && (step.with?.prompt === undefined || remove.some(path => path[0] === 'with' && path[1] === 'prompt'))) set.push({ path: ['with', 'prompt'], value: '' });
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
