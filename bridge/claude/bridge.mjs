import { query, tool, createSdkMcpServer } from '@anthropic-ai/claude-agent-sdk';
import { z } from 'zod';
import { createInterface } from 'node:readline';
import { readFile } from 'node:fs/promises';

const sdkVersion = '0.3.311';
const sdkMetadata = JSON.parse(await readFile(new URL('./package.json', import.meta.resolve('@anthropic-ai/claude-agent-sdk')), 'utf8'));
if (sdkMetadata.version !== sdkVersion) throw new Error('Pinned Claude SDK version mismatch before turn effects');

// Protocol v2 keeps stdin open for correlated callbacks and interruption.
const emit = value => process.stdout.write(JSON.stringify({ version: 2, ...value }) + '\n');
const lines = createInterface({ input: process.stdin, crlfDelay: Infinity });
const pending = new Map();
const toolIDs = new Map();
const nativePermits = new Map();
const nativeActive = new Set();
let expected = new Set();
let batchKnown = false;
let modelIdle = false;
let start;
let running;
let sessionID = '';
let terminal = false;
let interrupted = false;
let partialUsage;
const requestReady = new Promise((resolve, reject) => { start = { resolve, reject }; });

function barrier() {
  emit({ type: 'progress', data: { type: 'broker_wait_state', pending_count: pending.size, native_active: nativeActive.size, model_idle: modelIdle } });
  // Custom tools explicitly use the pinned SDK's serial (non-read-only) path.
  // Later unstarted tools cannot run until this callback returns. Every tool
  // already started must await the broker or have finished; native work keeps
  // the execution lease occupied. The full assistant batch must be known.
  if (modelIdle && batchKnown && pending.size && !nativePermits.size && !nativeActive.size && [...pending.keys()].every(id => expected.has(id))) {
    emit({ type: 'blocked', call_ids: [...pending.keys()] });
  }
}
function rejectPending(error) { for (const promise of [...pending.values(), ...nativePermits.values()]) promise.reject(error); pending.clear(); nativePermits.clear(); }

lines.on('line', line => {
  try {
    if (Buffer.byteLength(line) > 16 * 1024 * 1024) throw new Error('bridge frame exceeds 16 MiB');
    const frame = JSON.parse(line);
    if (frame.version !== 2) throw new Error('bridge protocol version mismatch');
    if (frame.type === 'turn_start') {
      if (!start) throw new Error('duplicate turn start');
      const resolve = start.resolve; start = undefined; resolve(frame);
    } else if (frame.type === 'tool_result') {
      const callback = pending.get(frame.call_id);
      if (!callback) throw new Error('unknown tool result identity');
      pending.delete(frame.call_id);
      if (frame.error) callback.resolve({ content: [{ type: 'text', text: frame.error }], isError: true });
      else callback.resolve({ content: [{ type: 'text', text: JSON.stringify(frame.result) }] });
    } else if (frame.type === 'interrupt') {
      interrupted = true; rejectPending(new Error('turn interrupted'));
      if (running) void running.interrupt().then(() => emit({ type: 'interrupted', session_id: sessionID })).catch(() => {});
    } else if (frame.type === 'native_permit') {
      const permit = nativePermits.get(frame.call_id);
      if (!permit) throw new Error('unknown native tool permit');
      nativePermits.delete(frame.call_id);
      if (frame.error) permit.reject(new Error(frame.error));
      else { nativeActive.add(frame.call_id); permit.resolve(); }
    } else throw new Error('unknown bridge control frame');
  } catch (error) {
    if (start) { start.reject(error); start = undefined; }
    rejectPending(error); process.exitCode = 1; running?.close(); lines.close();
  }
});
lines.on('close', () => {
  if (start) { start.reject(new Error('controller disconnected before turn start')); start = undefined; }
  if (!terminal) { rejectPending(new Error('controller lifetime ended')); running?.close(); }
});

emit({ type: 'hello', provider_version: 'claude-agent-sdk@' + sdkVersion });
try {
  const request = await requestReady;
  sessionID = request.session_id ?? '';
  const nativeTools = request.repair_only ? [] : (request.tools ?? []);
  if (!Array.isArray(nativeTools) || nativeTools.some(t => ['agent', 'task', 'skill', 'toolsearch', 'teamcreate', 'teamdelete', 'sendmessage'].includes(String(t).toLowerCase().replace(/[_-]/g, '')))) throw new Error('native delegation, discovery, and skills are disabled');
  const catalog = request.repair_only ? [] : (request.broker_tools ?? []);
  const brokerNames = new Set();
  const customTools = catalog.map(definition => {
    const name = definition.wire_name;
    const qualified = 'mcp__broker__' + name;
    if (typeof name !== 'string' || !/^broker_[a-f0-9]{24}$/.test(name) || brokerNames.has(qualified)) throw new Error('invalid broker catalog identity');
    brokerNames.add(qualified);
    const schema = z.fromJSONSchema(definition.input_schema);
    if (!(schema instanceof z.ZodObject)) throw new Error('broker input schema must be an object');
    return tool(name, definition.description, schema.shape, async args => {
      // The upstream model tool-use ID is recorded by PreToolUse. A callback
      // never manufactures a new identity when the provider repeats a call.
      const identities = toolIDs.get(qualified + '\0' + canonical(args));
      const callID = identities?.shift();
      if (!callID || pending.has(callID)) throw new Error('broker callback lacks a stable tool-use identity');
      return new Promise((resolve, reject) => {
        pending.set(callID, { resolve, reject });
        emit({ type: 'tool_request', call_id: callID, server: definition.server, tool: definition.name, arguments: args });
        setImmediate(barrier);
      });
    }, { annotations: { readOnlyHint: false } });
  });
  const allowed = [...nativeTools, ...brokerNames];
  // The pinned SDK synthesizes this formatting-only tool for outputFormat.
  if (request.schema) allowed.push('StructuredOutput');
  const postTool = async input => {
    if (nativeActive.delete(input.tool_use_id)) emit({ type: 'native_complete', call_id: input.tool_use_id });
    setImmediate(barrier); return {};
  };
  const options = {
    cwd: request.workspace, title: 'Agent Workflow', model: request.model || undefined, resume: sessionID || undefined,
    systemPrompt: request.instructions || 'Follow the explicitly supplied task and engine policy.',
    settingSources: [], settings: { autoMemoryEnabled: false, autoDreamEnabled: false },
    extraArgs: { 'disable-slash-commands': null, 'strict-mcp-config': null, 'no-chrome': null },
    skills: [], plugins: [], agents: {},
    mcpServers: customTools.length ? { broker: createSdkMcpServer({ name: 'broker', version: '2.0.0', tools: customTools }) } : {},
    tools: nativeTools, allowedTools: allowed, permissionMode: 'default', persistSession: !request.repair_only,
    includePartialMessages: true,
    hooks: {
      PreToolUse: [{ hooks: [async input => {
        if (!allowed.includes(input.tool_name)) return { hookSpecificOutput: { hookEventName: 'PreToolUse', permissionDecision: 'deny', permissionDecisionReason: 'Outside engine tool policy' } };
        if (brokerNames.has(input.tool_name)) {
          const key = input.tool_name + '\0' + canonical(input.tool_input);
          const ids = toolIDs.get(key) ?? []; ids.push(input.tool_use_id); toolIDs.set(key, ids);
        } else {
          // Every native operation obtains the invocation lease before its
          // hook permits effects, including work scheduled after a broker wait.
          await new Promise((resolve, reject) => {
            nativePermits.set(input.tool_use_id, { resolve, reject });
            emit({ type: 'native_request', call_id: input.tool_use_id, tool: input.tool_name });
          });
        }
        return {};
      }] }],
      PostToolUse: [{ hooks: [postTool] }], PostToolUseFailure: [{ hooks: [postTool] }],
    },
    canUseTool: async (name, input) => allowed.includes(name) ? { behavior: 'allow', updatedInput: input } : { behavior: 'deny', message: 'Tool is outside the engine role policy' },
  };
  if (request.schema) options.outputFormat = { type: 'json_schema', schema: request.schema };
  let finishInput;
  const inputDone = new Promise(resolve => { finishInput = resolve; });
  async function* prompt() {
    yield { type: 'user', session_id: sessionID, message: { role: 'user', content: request.prompt }, parent_tool_use_id: null };
    await inputDone;
  }
  running = query({ prompt: prompt(), options });
  if (interrupted) await running.interrupt();
  try {
    for await (const message of running) {
      if (message.session_id) { sessionID = message.session_id; emit({ type: 'session', session_id: sessionID }); }
      if (message.type === 'assistant') {
        emit({ type: 'progress', data: { type: 'assistant_batch', tool_ids: (message.message?.content ?? []).filter(item => item.type === 'tool_use').map(item => item.id), stop_reason: message.message?.stop_reason } });
        // The SDK emits one assistant envelope per content block. Accumulate
        // tool IDs; replacing the set loses earlier calls in the same batch.
        for (const item of message.message?.content ?? []) if (item.type === 'tool_use') expected.add(item.id);
        batchKnown = true;
        if (message.message?.usage) { partialUsage = message.message.usage; emit({ type: 'usage', session_id: sessionID, data: { usage: partialUsage, observation_key: message.message.id ?? 'counter' } }); }
        setImmediate(barrier);
      }
      if (message.type === 'stream_event') {
        if (message.event?.type === 'message_start') modelIdle = false;
        if (message.event?.type === 'message_stop') { modelIdle = true; setImmediate(barrier); }
      }
      if (message.type === 'result') {
        const succeeded = message.subtype === 'success' && !message.is_error;
        const text = message.structured_output !== undefined ? JSON.stringify(message.structured_output) : (message.result ?? '');
        terminal = true;
        emit({ type: 'result', session_id: sessionID, status: succeeded ? 'succeeded' : 'failed', provider_status: message.subtype, provider_version: 'claude-agent-sdk@0.3.311',
          outputs: { text, response: text, usage: message.usage, structured: message.structured_output, cost_usd: message.total_cost_usd }, error: succeeded ? undefined : (message.errors ?? [message.subtype]).join('; ') });
        break;
      }
      emit({ type: 'progress', session_id: sessionID, data: { type: message.type } });
    }
  } finally { finishInput(); }
  if (!terminal) throw new Error('SDK ended without terminal result');
} catch (error) {
  terminal = true;
  emit({ type: 'result', session_id: sessionID, status: 'failed', outputs: { usage: partialUsage, usage_incomplete: true }, error: String(error) }); process.exitCode = 1;
} finally { terminal = true; rejectPending(new Error('provider turn ended')); running?.close(); lines.close(); }

function canonical(value) {
  if (Array.isArray(value)) return '[' + value.map(canonical).join(',') + ']';
  if (value && typeof value === 'object') return '{' + Object.keys(value).sort().map(key => JSON.stringify(key) + ':' + canonical(value[key])).join(',') + '}';
  return JSON.stringify(value);
}
