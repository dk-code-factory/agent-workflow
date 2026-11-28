// This validator is specific to the locked native 2.1.322 streaming executor.
// The serial MCP callback stays held; a message_stop alone grants no authority.
export class BrokerStream {
  constructor(names) { this.names = names; this.seen = new Set(); this.current = undefined; }
  event(event) {
    if (event.type === 'message_start') {
      if (this.current && (!this.current.proof || this.current.remaining.size)) throw new Error('Model work overlaps an unresolved original broker batch');
      const message = event.message;
      if (!message || typeof message.id !== 'string' || !message.id || message.id.length > 256 || this.seen.has(message.id) || this.seen.size >= 4096) throw new Error('Invalid original model response identity');
      this.seen.add(message.id);
      this.current = { id: message.id, model: message.model, usage: { ...message.usage }, blocks: new Map(), remaining: new Set() };
      return;
    }
    const batch = this.current;
    if (!batch) throw new Error('Stream event precedes original model response');
    if (batch.proof) throw new Error('Stream data follows sealed original response');
    if (event.type === 'content_block_start') {
      if (batch.final || !Number.isSafeInteger(event.index) || event.index < 0 || batch.blocks.has(event.index) || batch.blocks.size >= 128) throw new Error('Invalid original content block');
      const block = event.content_block;
      if (!block || !['text', 'thinking', 'redacted_thinking', 'tool_use'].includes(block.type)) throw new Error('Unsupported original content block');
      if (block.type === 'tool_use' && (typeof block.id !== 'string' || !block.id || block.id.length > 256 || !this.names.has(block.name))) throw new Error('Original model planned an unadmitted tool');
      batch.blocks.set(event.index, { ...block, json: '', stopped: false });
    } else if (event.type === 'content_block_delta') {
      const block = batch.blocks.get(event.index);
      if (!block || block.stopped || batch.final) throw new Error('Unplanned original content delta');
      if (block.type === 'tool_use') {
        if (event.delta?.type !== 'input_json_delta' || typeof event.delta.partial_json !== 'string' || block.json.length + event.delta.partial_json.length > 1 << 20) throw new Error('Invalid original broker arguments');
        block.json += event.delta.partial_json;
      }
    } else if (event.type === 'content_block_stop') {
      const block = batch.blocks.get(event.index);
      if (!block || block.stopped || batch.final) throw new Error('Invalid original block completion');
      block.stopped = true;
    } else if (event.type === 'message_delta') {
      if (batch.final || !['tool_use', 'end_turn', 'max_tokens', 'stop_sequence'].includes(event.delta?.stop_reason) || !Object.hasOwn(event.usage ?? {}, 'output_tokens')) throw new Error('Missing original final delta/usage');
      batch.final = event.delta.stop_reason;
      batch.usage = { ...batch.usage, ...event.usage };
    } else if (event.type === 'message_stop') {
      if (!batch.final || [...batch.blocks.values()].some(block => !block.stopped)) throw new Error('Synthetic or incomplete model stop');
      const usage = {};
      for (const name of ['input_tokens', 'output_tokens', 'cache_read_input_tokens', 'cache_creation_input_tokens']) {
        const value = batch.usage[name] ?? (name.startsWith('cache_') ? 0 : undefined);
        if (!Number.isSafeInteger(value) || value < 0) throw new Error('Unknown original response usage');
        usage[name] = value;
      }
      const calls = [];
      for (const block of batch.blocks.values()) if (block.type === 'tool_use') {
        if (batch.remaining.has(block.id)) throw new Error('Duplicate original broker identity');
        const args = block.json ? JSON.parse(block.json) : block.input;
        if (!args || Array.isArray(args) || typeof args !== 'object') throw new Error('Original broker arguments must be an object');
        batch.remaining.add(block.id); calls.push({ id: block.id, name: block.name, arguments: args });
      }
      if (Boolean(calls.length) !== (batch.final === 'tool_use')) throw new Error('Original stop reason differs from planned tools');
      batch.proof = { response_id: batch.id, model: batch.model, stop_reason: batch.final, usage, calls };
      return batch.proof;
    } else if (event.type !== 'ping') throw new Error('Unsupported original stream event');
  }
  settle(id) {
    if (!this.current?.proof || !this.current.remaining.delete(id)) throw new Error('Unplanned or repeated broker result');
  }
  blocked(ids) {
    const batch = this.current;
    if (!batch?.proof || ids.length !== 1 || !batch.remaining.has(ids[0])) return undefined;
    return batch.proof;
  }
}
