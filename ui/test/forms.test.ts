import { describe, expect, it } from 'vitest';
import { InspectorDrafts } from '../src/forms';

describe('inspector bulk aggregation', () => {
  it('does linear pending work while discarding clean or pending fields', () => {
    for (const lastPending of [false, true]) {
      let scans = 0; const notifications: boolean[] = [];
      const registry = new InspectorDrafts(value => notifications.push(value));
      for (let index = 0; index < 200; index++) registry.register({ identifier: false, apply: () => true, reveal: () => {},
        isPending: () => { scans++; return lastPending && index === 199; }, discard: () => registry.changed() });
      scans = 0; notifications.length = 0; registry.discard();
      expect(scans).toBe(200); expect(notifications).toEqual([lastPending]);
      registry.changed(); expect(scans).toBe(400); expect(notifications).toHaveLength(2);
    }
  });
  it('restores aggregation after nested failure and ignores retired callbacks', () => {
    const notifications: boolean[] = []; const calls: string[] = []; let nested = false; let pending = true;
    const registry = new InspectorDrafts(value => notifications.push(value));
    const retire = registry.register({ identifier: false, apply: () => true, reveal: () => {}, isPending: () => false, discard: () => { calls.push('retired'); } });
    registry.register({ custom: true, identifier: false, apply: () => true, reveal: () => {}, isPending: () => pending, discard: () => {
      registry.changed(); retire();
      if (!nested) { nested = true; registry.discard(); throw new Error('Host failure'); }
    } });
    notifications.length = 0;
    expect(() => registry.discard()).toThrow('Host failure');
    expect(notifications).toEqual([true]); expect(calls).toEqual([]);
    pending = false; registry.changed(); expect(notifications).toEqual([true, false]);
  });
});
