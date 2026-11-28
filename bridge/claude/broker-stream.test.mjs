import { test } from 'node:test';
import assert from 'node:assert/strict';
import { BrokerStream } from './broker-stream.mjs';
const name = 'mcp__broker__broker_123456789012345678901234';
const start = (s, id = 'response') => s.event({ type: 'message_start', message: { id, model: 'model', usage: { input_tokens: 11, output_tokens: 0 } } });
function planned(s, id = 'call', index = 0, tool = name) {
  s.event({ type: 'content_block_start', index, content_block: { type: 'tool_use', id, name: tool, input: {} } });
  s.event({ type: 'content_block_delta', index, delta: { type: 'input_json_delta', partial_json: '{"query":"original"}' } });
  s.event({ type: 'content_block_stop', index });
}
const final = s => s.event({ type: 'message_delta', delta: { stop_reason: 'tool_use' }, usage: { output_tokens: 3 } });
test('complete immutable original plan gates both serial callbacks and the next response', () => {
  const s = new BrokerStream(new Set([name])); start(s); planned(s); planned(s, 'second', 1);
  assert.equal(s.blocked(['call']), undefined); final(s); assert.equal(s.blocked(['call']), undefined);
  const proof = s.event({ type: 'message_stop' });
  assert.deepEqual(proof.calls.map(call => call.id), ['call', 'second']); assert.equal(proof.usage.output_tokens, 3);
  assert.equal(s.blocked(['call']), proof); s.settle('call'); assert.equal(s.blocked(['second']), proof);
  assert.throws(() => start(s, 'overlap'), /unresolved/); s.settle('second'); start(s, 'next');
  planned(s, 'third'); final(s); assert.equal(s.event({ type: 'message_stop' }).calls[0].id, 'third');
});
test('synthetic stop, omitted final usage, unadmitted tool and trailing events stay closed', () => {
  for (const mutate of [s => s.event({ type: 'message_stop' }), s => s.event({ type: 'message_delta', delta: { stop_reason: 'tool_use' } }), s => planned(s, 'unknown', 0, 'Bash')]) {
    const s = new BrokerStream(new Set([name])); start(s); assert.throws(() => mutate(s)); assert.equal(s.blocked(['call']), undefined);
  }
  const s = new BrokerStream(new Set([name])); start(s); planned(s); final(s); s.event({ type: 'message_stop' });
  assert.throws(() => planned(s, 'late', 1), /sealed/); assert.throws(() => s.settle('unknown'), /Unplanned/); s.settle('call'); assert.throws(() => s.settle('call'), /repeated/);
});
test('unfinished block rejects a valid final delta and stop without granting a callback', () => {
  const s = new BrokerStream(new Set([name])); start(s);
  s.event({ type: 'content_block_start', index: 0, content_block: { type: 'tool_use', id: 'call', name, input: {} } });
  s.event({ type: 'content_block_delta', index: 0, delta: { type: 'input_json_delta', partial_json: '{"query":"original"}' } });
  final(s);
  assert.throws(() => s.event({ type: 'message_stop' }), /incomplete model stop/);
  assert.equal(s.blocked(['call']), undefined);
});
test('repeated IDs, unknown counters and extra model response are rejected', () => {
  const s = new BrokerStream(new Set([name])); start(s); planned(s); planned(s, 'call', 1); final(s); assert.throws(() => s.event({ type: 'message_stop' }), /Duplicate/);
  const t = new BrokerStream(new Set([name])); start(t); planned(t); t.event({ type: 'message_delta', delta: { stop_reason: 'tool_use' }, usage: { output_tokens: -1 } }); assert.throws(() => t.event({ type: 'message_stop' }), /usage/);
  const u = new BrokerStream(new Set([name])); start(u); assert.throws(() => start(u, 'second'), /unresolved/);
});

test('prototype-looking original keys stay data and cannot mutate authority', () => {
  const s = new BrokerStream(new Set([name])); start(s);
  s.event({ type: 'content_block_start', index: 0, content_block: { type: 'tool_use', id: 'original', name, input: {} } });
  s.event({ type: 'content_block_delta', index: 0, delta: { type: 'input_json_delta', partial_json: '{"__proto__":{"authority":true},"query":"original"}' } });
  s.event({ type: 'content_block_stop', index: 0 }); final(s);
  const proof = s.event({ type: 'message_stop' });
  assert.equal(Object.hasOwn(proof.calls[0].arguments, '__proto__'), true);
  assert.equal(Object.getPrototypeOf(proof.calls[0].arguments), Object.prototype);
  assert.equal({}.authority, undefined);
  assert.equal(s.names.has('Bash'), false);
});
