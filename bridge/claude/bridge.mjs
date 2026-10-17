import { query } from '@anthropic-ai/claude-agent-sdk';

// One turn per supervised bridge process. SDK transcripts persist in the
// engine-owned config directory, and subsequent turns supply the session ID.
let input = '';
for await (const chunk of process.stdin) {
  input += chunk;
  if (Buffer.byteLength(input) > 4 * 1024 * 1024) throw new Error('bridge request exceeds 4 MiB');
}
const request = JSON.parse(input);
const emit = value => process.stdout.write(JSON.stringify({version: 1, ...value}) + '\n');
const tools = request.repair_only ? [] : (request.tools ?? []);
if (!Array.isArray(tools) || tools.some(t => ['agent', 'task', 'skill', 'toolsearch', 'teamcreate', 'teamdelete', 'sendmessage'].includes(String(t).toLowerCase().replace(/[_-]/g, '')))) {
  throw new Error('native delegation, discovery, and skills are disabled');
}
let sessionID = request.session_id ?? '';
try {
  const options = {
    cwd: request.workspace,
    title: "Agent Workflow",
    model: request.model || undefined,
    resume: sessionID || undefined,
    systemPrompt: request.instructions || 'Follow the explicitly supplied task and engine policy.',
    settingSources: [],
    settings: {autoMemoryEnabled: false, autoDreamEnabled: false},
    extraArgs: {"disable-slash-commands": null, "strict-mcp-config": null, "no-chrome": null},
    skills: [],
    plugins: [],
    agents: {},
    mcpServers: {},
    tools,
    allowedTools: tools,
    permissionMode: 'default',
    persistSession: !request.repair_only,
    canUseTool: async (name, input) => tools.includes(name)
      ? {behavior: 'allow', updatedInput: input}
      : {behavior: 'deny', message: 'Tool is outside the engine role policy'},
  };
  if (request.schema) options.outputFormat = {type: 'json_schema', schema: request.schema};
  for await (const message of query({prompt: request.prompt, options})) {
    if (message.session_id) {
      sessionID = message.session_id;
      emit({type: 'session', session_id: sessionID});
    }
    if (message.type === 'result') {
      const succeeded = message.subtype === 'success' && !message.is_error;
      const text = message.structured_output !== undefined
        ? JSON.stringify(message.structured_output) : (message.result ?? '');
      emit({type: 'result', session_id: sessionID, status: succeeded ? 'succeeded' : 'failed', provider_status: message.subtype, provider_version: 'claude-agent-sdk@0.3.301',
        outputs: {text, response: text, usage: message.usage, structured: message.structured_output, cost_usd: message.total_cost_usd},
        error: succeeded ? undefined : (message.errors ?? [message.subtype]).join('; ')});
    } else {
      emit({type: 'progress', session_id: sessionID, data: {type: message.type}});
    }
  }
} catch (error) {
  emit({type: 'result', session_id: sessionID, status: 'failed', error: String(error)});
  process.exitCode = 1;
}
