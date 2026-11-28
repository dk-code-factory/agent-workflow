// Pinned Claude Code 2.1.322 builtins. Unknown additions are rejected using the
// actual system/init plugin set, rather than trusting this list alone.
export const isolatedSettings = {
  autoMemoryEnabled: false,
  autoDreamEnabled: false,
  autoCompactEnabled: false,
  precomputeCompactionEnabled: false,
  idleCompaction: false,
  enabledPlugins: Object.fromEntries([
    'cc-plugin-agents-md', 'cc-plugin-telemetry', 'cc-plugin-privacy',
    'cc-plugin-plugin-authoring', 'cc-plugin-easel', 'cc-plugin-sec-default', 'sec-default',
  ].map(name => [name + '@builtin', false])),
};

export function assertIsolatedInitialization(message) {
  if (!Array.isArray(message.plugins) || message.plugins.length ||
      !Array.isArray(message.skills) || message.skills.length ||
      !Array.isArray(message.tools) || message.tools.some(name =>
        ['skill', 'agent', 'task', 'toolsearch', 'teamcreate', 'teamdelete', 'sendmessage']
          .includes(String(name).toLowerCase().replace(/[_-]/g, ''))) ||
      (message.plugin_errors !== undefined && (!Array.isArray(message.plugin_errors) || message.plugin_errors.length))) {
    throw new Error('Provider initialization violates engine skill/plugin isolation');
  }
}

export function assertNativeTaskPolicy(policy, tools) {
  if (!policy || policy.mode !== 'read-only' || policy.provider !== 'claude-sdk' ||
      policy.profile !== 'claude-sdk@0.3.322/native-task-read-only-v1' ||
      !Array.isArray(policy.native_tools) || !Array.isArray(tools) ||
      policy.native_tools.some(name => !['Read', 'Glob', 'Grep'].includes(name)) ||
      new Set(policy.native_tools).size !== policy.native_tools.length ||
      JSON.stringify(policy.native_tools) !== JSON.stringify(tools)) {
    throw new Error('Invalid admitted native task read-only policy');
  }
}

export function assertNativeTaskAcknowledgement(initialization) {
  if (initialization.claude_code_version !== '2.1.322' || initialization.hooks_applied !== true ||
      initialization.hook_on_failure_supported !== true) {
    throw new Error('Provider did not initialize the native task read-only controls');
  }
}

export function assertNativeTaskInitialization(message, policy, formattingTool) {
  assertIsolatedInitialization(message);
  const expected = [...policy.native_tools, ...(formattingTool ? ['StructuredOutput'] : [])];
  if (message.tools.length !== expected.length || new Set(message.tools).size !== expected.length ||
      message.tools.some(name => !expected.includes(name))) {
    throw new Error('Provider initialization exceeds the native task read-only grant');
  }
}

// Initialization acknowledgement alone is insufficient: the pinned CLI also
// returns plugins_applied:true for missing/corrupt local plugin manifests.
export function assertSkillAcknowledgement(initialization, qualified) {
  if (initialization.claude_code_version !== '2.1.322' || initialization.plugins_applied !== true ||
      initialization.hooks_applied !== true || initialization.hook_on_failure_supported !== true ||
      !Array.isArray(initialization.commands) || !initialization.commands.some(command => command.name === qualified)) {
    throw new Error('Provider did not initialize the admitted skill before prompt release');
  }
}

export function assertSelectedInitialization(message, skill) {
  const expectedName = 'aw-' + skill.name;
  if (!Array.isArray(message.plugins) || message.plugins.length !== 1 ||
      message.plugins[0].name !== expectedName || message.plugins[0].path !== skill.root ||
      !Array.isArray(message.skills) || !message.skills.includes(skill.qualified) ||
      !Array.isArray(message.tools) || !message.tools.includes('Skill') ||
      message.tools.some(name => ['agent', ...(skill.child ? [] : ['task']), 'toolsearch', 'teamcreate', 'teamdelete', 'sendmessage'].includes(String(name).toLowerCase().replace(/[_-]/g, ''))) ||
      // This exact CLI reports its model-facing Agent tool as Task in init.
      // Hooks/model requests still use Agent; only that pinned alias is allowed.
      (Boolean(skill.child) && (message.tools.length !== 3 || !['Task', 'Read', 'Skill'].every(name => message.tools.includes(name)))) ||
      (message.plugin_errors !== undefined && (!Array.isArray(message.plugin_errors) || message.plugin_errors.length))) {
    throw new Error('Provider initialization differs from admitted skill/plugin set');
  }
}

// Native 2.1.322 checks this ceiling before automatic, idle, precomputed,
// manual and plugin compaction. Settings alone are not the execution ceiling.
export const isolatedEnvironment = {
  DISABLE_COMPACT: '1',
  DISABLE_AUTO_COMPACT: '1',
};
