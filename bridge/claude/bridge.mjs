import { query, tool, createSdkMcpServer } from '@anthropic-ai/claude-agent-sdk';
import { z } from 'zod';
import { createInterface } from 'node:readline';
import { readFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { isolatedSettings, assertIsolatedInitialization, assertSkillAcknowledgement, assertSelectedInitialization } from './isolation.mjs';

const sdkVersion = '0.3.322';
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
let attemptEvidence;
let isolatedInitialization = false;
let releasePrompt;
const promptReady = new Promise(resolve => { releasePrompt = resolve; });
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
  const skill = request.skill;
  const child = skill?.child;
  if (skill && (request.repair_only || !['claude-sdk@0.3.322/local-skill-v1', 'claude-sdk@0.3.322/sonnet46-one-read-child-v1'].includes(skill.profile) || Boolean(child) !== (skill.profile === 'claude-sdk@0.3.322/sonnet46-one-read-child-v1') || skill.qualified !== 'aw-' + skill.name + ':' + skill.name || typeof skill.root !== 'string' || typeof skill.arguments !== 'string')) throw new Error('Invalid admitted skill contract');
  if (child && (request.model !== 'claude-sonnet-4-6' || child.model !== request.model || child.effort !== 'high' || child.type !== 'aw-child-' + child.name || child.max_turns !== 2 || child.max_calls !== 1 || child.background !== false || typeof child.prompt !== 'string' || !child.prompt || typeof child.description !== 'string' || !child.description || !Array.isArray(child.resources) || !child.resources.length)) throw new Error('Invalid admitted child controls');
  const skillCalls = new Set();
  const parentAgentMessages = new Map();
  let skillSucceeded = false;
  let childCall = '';
  let childID = '';
  let childStarted = false;
  const bindChild = id => {
    if (!id || childID && childID !== id) throw new Error('Native child identity changed');
    childID = id;
  };
  const childEvidence = (kind, raw, id = childID) => {
    if (!child || !childCall) throw new Error('Native child event lacks admitted parent call');
    const evidence = { key: kind + ':' + createHash('sha256').update(canonical(raw)).digest('hex'), kind, session_id: raw.session_id, parent_call_id: childCall, child_id: id || undefined, name: child.name, type: child.type, raw };
    emit({ type: 'skill_child_evidence', session_id: raw.session_id, data: evidence });
    return evidence;
  };
  const nativeTools = request.repair_only ? [] : (request.tools ?? []);
  if (!Array.isArray(nativeTools) || nativeTools.some(t => [...(child ? [] : ['agent']), 'task', ...(skill ? [] : ['skill']), 'toolsearch', 'teamcreate', 'teamdelete', 'sendmessage'].includes(String(t).toLowerCase().replace(/[_-]/g, '')))) throw new Error('native delegation, discovery, and skills are disabled');
  const catalog = request.repair_only ? [] : (request.broker_tools ?? []);
  const brokerNames = new Set();
  const customTools = catalog.map(definition => {
    const name = definition.wire_name;
    const qualified = 'mcp__broker__' + name;
    if (typeof name !== 'string' || !/^broker_[a-f0-9]{24}$/.test(name) || brokerNames.has(qualified)) throw new Error('invalid broker catalog identity');
    brokerNames.add(qualified);
    // Keep the SDK-exposed object fields for controller validation. The SDK
    // normalizes model input before this boundary; this is not wire fidelity.
    const schema = definition.controller_validation ? z.looseObject({}) : z.fromJSONSchema(definition.input_schema);
    if (!(schema instanceof z.ZodObject)) throw new Error('broker input schema must be an object');
    const description = definition.controller_validation ? definition.description + '\nController argument contract: ' + JSON.stringify(definition.input_schema) : definition.description;
    return tool(name, description, definition.controller_validation ? schema : schema.shape, async args => {
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
  if (child && (catalog.length || nativeTools.some(tool => !['Read', 'Skill', 'Agent'].includes(tool)))) throw new Error('Admitted child exceeds parent native authority');
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
    settingSources: [], settings: isolatedSettings,
    extraArgs: { ...(skill ? {} : { 'disable-slash-commands': null }), 'strict-mcp-config': null, 'no-chrome': null },
    pluginDelivery: skill ? 'initialize' : 'argv',
    skills: skill ? [skill.qualified] : [], plugins: skill ? [{ type: 'local', path: skill.root, skipMcpDiscovery: true }] : [],
    agents: child ? { [child.type]: { description: child.description, prompt: 'Use only the explicitly retained resource files. Never delegate.\n' + child.resources.join('\n'), tools: ['Read'], disallowedTools: ['Agent', 'Task', 'Skill', 'Bash', 'ToolSearch', 'TeamCreate', 'TeamDelete', 'SendMessage'], model: 'inherit', effort: 'high', skills: [], maxTurns: 2, background: false, omitClaudeMd: true, mcpServers: [] } } : {},
    mcpServers: customTools.length ? { broker: createSdkMcpServer({ name: 'broker', version: '2.0.0', tools: customTools }) } : {},
    tools: nativeTools, allowedTools: allowed, permissionMode: 'default', persistSession: !request.repair_only,
    includePartialMessages: true,
    // process.env is already the controller's private, allowlisted bridge
    // environment. The SDK replaces env rather than merging it implicitly.
    ...(skill ? { env: { ...process.env, CLAUDE_CODE_MAX_RETRIES: '0', CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK: '1', CLAUDE_CODE_NONSTREAMING_TIMEOUT_RETRIES: '0' } } : {}),
    ...(child ? { effort: 'high', maxTurns: 4, includeHookEvents: true, forwardSubagentText: true } : {}),
    hooks: {
      PreToolUse: [{ ...(skill ? { onFailure: 'block' } : {}), hooks: [async input => {
        if (!isolatedInitialization) return { hookSpecificOutput: { hookEventName: 'PreToolUse', permissionDecision: 'deny', permissionDecisionReason: 'Provider initialization is not verified' } };
        let permitEvidence;
        if (input.agent_id) {
          if (!child || !childCall || childID && input.agent_id !== childID || input.agent_type !== child.type || input.tool_name !== 'Read' || !child.resources.includes(input.tool_input?.file_path) || Object.keys(input.tool_input).some(key => key !== 'file_path') || input.effort?.level !== 'high') return { hookSpecificOutput: { hookEventName: 'PreToolUse', permissionDecision: 'deny', permissionDecisionReason: 'Outside admitted child resource/tool controls' } };
          bindChild(input.agent_id);
        } else if (input.tool_name === 'Agent') {
          const args = input.tool_input;
          const parentMessage = parentAgentMessages.get(input.tool_use_id);
          const original = parentMessage?.message?.content?.find(item => item.type === 'tool_use' && item.id === input.tool_use_id && item.name === 'Agent');
          if (!child || skill.child_complete || childCall || !skillSucceeded || input.effort?.level !== 'high' || !args || args.subagent_type !== child.type || args.prompt !== child.prompt || args.description !== child.description || args.run_in_background !== false || Object.keys(args).length !== 4 || Object.keys(args).some(key => !['subagent_type', 'prompt', 'description', 'run_in_background'].includes(key)) || !original || canonical(original.input) !== canonical(args)) return { hookSpecificOutput: { hookEventName: 'PreToolUse', permissionDecision: 'deny', permissionDecisionReason: 'Outside original admitted foreground child allowance' } };
          childCall = input.tool_use_id;
          // The controller journals this original hook and consumes its durable
          // allowance before returning native_permit. No event-only permission.
          permitEvidence = { key: 'permit:' + input.tool_use_id, kind: 'permit', session_id: input.session_id, parent_call_id: input.tool_use_id, name: child.name, type: child.type, raw: input, parent_message: parentMessage };
        }
        if (input.tool_name === 'Skill') {
          const args = input.tool_input;
          if (!skill || !args || args.skill !== skill.qualified || (args.args ?? '') !== skill.arguments || Object.keys(args).some(key => !['skill', 'args'].includes(key))) return { hookSpecificOutput: { hookEventName: 'PreToolUse', permissionDecision: 'deny', permissionDecisionReason: 'Skill invocation differs from admitted identity/options' } };
          skillCalls.add(input.tool_use_id);
          emit({ type: 'skill_evidence', session_id: input.session_id, data: { key: 'invoke:' + input.tool_use_id, kind: 'invocation', session_id: input.session_id, call_id: input.tool_use_id, name: skill.name, arguments: skill.arguments, raw: input } });
        }
        if (!allowed.includes(input.tool_name)) return { hookSpecificOutput: { hookEventName: 'PreToolUse', permissionDecision: 'deny', permissionDecisionReason: 'Outside engine tool policy' } };
        if (brokerNames.has(input.tool_name)) {
          const key = input.tool_name + '\0' + canonical(input.tool_input);
          const ids = toolIDs.get(key) ?? []; ids.push(input.tool_use_id); toolIDs.set(key, ids);
        } else {
          // Every native operation obtains the invocation lease before its
          // hook permits effects, including work scheduled after a broker wait.
          await new Promise((resolve, reject) => {
            nativePermits.set(input.tool_use_id, { resolve, reject });
            emit({ type: 'native_request', call_id: input.tool_use_id, tool: input.tool_name, ...(permitEvidence ? { data: permitEvidence } : {}) });
          });
        }
        return {};
      }] }],
      PostToolUse: [{ hooks: [postTool] }], PostToolUseFailure: [{ hooks: [postTool] }],
      ...(child ? {
        SubagentStart: [{ onFailure: 'block', hooks: [async input => {
          if (!childCall || childStarted || input.agent_type !== child.type || !input.agent_id) throw new Error('Unadmitted native child start');
          bindChild(input.agent_id); childStarted = true; childEvidence('start', input); return {};
        }] }],
        SubagentStop: [{ onFailure: 'block', hooks: [async input => {
          if (input.agent_id !== childID || input.agent_type !== child.type || input.effort?.level !== 'high') throw new Error('Unadmitted native child stop');
          childEvidence('stop', input); return {};
        }] }],
      } : {}),
    },
    canUseTool: async (name, input) => allowed.includes(name) ? { behavior: 'allow', updatedInput: input } : { behavior: 'deny', message: 'Tool is outside the engine role policy' },
  };
  if (request.schema) options.outputFormat = { type: 'json_schema', schema: request.schema };
  let finishInput;
  const inputDone = new Promise(resolve => { finishInput = resolve; });
  async function* prompt() {
    if (skill && !await promptReady) return;
    yield { type: 'user', session_id: sessionID, message: { role: 'user', content: request.prompt }, parent_tool_use_id: null };
    await inputDone;
  }
  running = query({ prompt: prompt(), options });
  if (skill) {
    const initialization = await running.initializationResult();
    assertSkillAcknowledgement(initialization, skill.qualified);
    emit({ type: 'progress', data: { type: 'skill_acknowledged', qualified: skill.qualified } });
    releasePrompt(true);
  }
  if (interrupted) await running.interrupt();
  try {
    for await (const message of running) {
      if (message.type === 'system' && message.subtype === 'init') {
        if (skill) assertSelectedInitialization(message, skill); else assertIsolatedInitialization(message);
        isolatedInitialization = true;
        emit({ type: 'progress', data: { type: 'discovery_isolation', plugins: message.plugins, skills: message.skills } });
      }
      if (message.session_id) { sessionID = message.session_id; emit({ type: 'session', session_id: sessionID }); }
      if (skill && (message.type === 'system' && message.subtype === 'api_retry' || message.subagent_retry || message.type === 'assistant' && message.error)) {
        attemptEvidence = message;
        emit({ type: 'skill_attempt_uncertainty', session_id: sessionID, data: { key: 'retry:' + createHash('sha256').update(canonical(message)).digest('hex'), session_id: sessionID, provider: 'claude-sdk', raw: message } });
        throw new Error('Native retry left original selected-skill usage/effects uncertain');
      }
      if (skill && message.parent_tool_use_id) {
        if (!child || message.parent_tool_use_id !== childCall || !['assistant', 'user'].includes(message.type)) throw new Error('Unexpected native child in skill session');
        childEvidence('message', message);
        // Child model/tool batches do not participate in the parent broker
        // barrier. The original execution lease remains held throughout.
        continue;
      }
      if (child && message.type === 'system' && String(message.subtype).startsWith('task_')) {
        if (message.tool_use_id !== childCall && !(message.subtype === 'task_updated' && message.tool_use_id === undefined)) throw new Error('Unexpected native child task identity');
        bindChild(message.task_id);
        childEvidence('task', message);
      }
      if (skill && message.type === 'user') {
        for (const result of message.message?.content ?? []) {
          if (result.type === 'tool_result' && skillCalls.has(result.tool_use_id) && result.is_error !== true && message.tool_use_result?.success === true && message.tool_use_result.commandName === skill.qualified) {
            skillSucceeded = true;
            emit({ type: 'skill_evidence', session_id: message.session_id, data: { key: 'result:' + result.tool_use_id, kind: 'result', session_id: message.session_id, call_id: result.tool_use_id, name: skill.name, arguments: skill.arguments, raw: message } });
          }
          if (child && result.type === 'tool_result' && result.tool_use_id === childCall) childEvidence('result', message);
        }
      }
      if (message.type === 'assistant') {
        if (child) for (const item of message.message?.content ?? []) if (item.type === 'tool_use' && item.name === 'Agent') {
          if (parentAgentMessages.has(item.id) && canonical(parentAgentMessages.get(item.id)) !== canonical(message)) throw new Error('Original parent Agent call identity changed');
          if (parentAgentMessages.size >= 128) throw new Error('Parent Agent call evidence exceeds retention limit');
          parentAgentMessages.set(item.id, message);
        }
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
        if (!isolatedInitialization) throw new Error('Provider omitted isolation initialization');
        const succeeded = message.subtype === 'success' && !message.is_error;
        const text = message.structured_output !== undefined ? JSON.stringify(message.structured_output) : (message.result ?? '');
        if (child && childCall) childEvidence('parent_terminal', message);
        terminal = true;
        emit({ type: 'result', session_id: sessionID, status: succeeded ? 'succeeded' : 'failed', provider_status: message.subtype, provider_version: 'claude-agent-sdk@' + sdkVersion,
          outputs: { text, response: text, usage: message.usage, structured: message.structured_output, cost_usd: message.total_cost_usd, ...(child ? { native_child_result: message } : {}) }, error: succeeded ? undefined : (message.errors ?? [message.subtype]).join('; ') });
        break;
      }
      emit({ type: 'progress', session_id: sessionID, data: { type: message.type } });
    }
  } finally { finishInput(); }
  if (!terminal) throw new Error('SDK ended without terminal result');
} catch (error) {
  terminal = true;
  emit({ type: 'result', session_id: sessionID, status: 'failed', outputs: { usage: partialUsage, usage_incomplete: true, ...(attemptEvidence ? { native_attempt_error: attemptEvidence } : {}) }, error: String(error) }); process.exitCode = 1;
} finally { terminal = true; releasePrompt(false); rejectPending(new Error('provider turn ended')); running?.close(); lines.close(); }

function canonical(value) {
  if (Array.isArray(value)) return '[' + value.map(canonical).join(',') + ']';
  if (value && typeof value === 'object') return '{' + Object.keys(value).sort().map(key => JSON.stringify(key) + ':' + canonical(value[key])).join(',') + '}';
  return JSON.stringify(value);
}
