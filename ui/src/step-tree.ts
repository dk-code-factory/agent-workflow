import type { Path, Step } from './types';

export const blockKind = (step: Step): 'parallel' | 'sequence' | undefined =>
  Object.hasOwn(step, 'parallel') ? 'parallel' : Object.hasOwn(step, 'sequence') ? 'sequence' : undefined;

export interface StepEntry {
  step: Step;
  /** Path relative to the job, including the initial steps key. */
  path: Path;
  id: string;
  /** Results completed before this step starts, excluding parallel siblings. */
  available: Set<string>;
}

export function stepEntries(steps: Step[] = []): StepEntry[] {
  const entries: StepEntry[] = [];
  const visit = (items: Step[], path: Path, prefix: string, incoming: Set<string>, parallel = false): Set<string> => {
    const completed = new Set(incoming);
    items.forEach((step, index) => {
      const fallback = `${prefix}${index + 1}`;
      const entry: StepEntry = { step, path: [...path, index], id: step.id || fallback, available: new Set(parallel ? incoming : completed) };
      entries.push(entry);
      const kind = blockKind(step);
      if (kind) for (const id of visit(step[kind] ?? [], [...entry.path, kind], `${fallback}_${kind}_`, entry.available, kind === 'parallel')) completed.add(id);
      completed.add(entry.id);
    });
    return completed;
  };
  visit(steps, ['steps'], 'step_', new Set());
  return entries;
}

export function containingStep(entries: StepEntry[], path: Path): StepEntry | undefined {
  for (let index = entries.length - 1; index >= 0; index--) {
    const entry = entries[index];
    if (entry.path.length <= path.length && entry.path.every((part, offset) => part === path[offset])) return entry;
  }
}

export function producerStep(steps: Step[] | undefined, selection: number | Path): StepEntry | undefined {
  const path = typeof selection === 'number' ? ['steps', selection] : selection;
  return stepEntries(steps).find(entry => entry.path.length === path.length && entry.path.every((part, index) => part === path[index]));
}
